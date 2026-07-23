// Package certs is the crypto core of the certificate manager: creating CAs,
// issuing and renewing leaf certificates, validating uploads, and generating
// age keypairs.
//
// Everything speaks PEM strings, because that is what the store holds
// (certificates in plaintext, private keys sealed) and what operators paste.
// Nothing in this package touches the database, the sealer, or Docker — it is
// pure crypto, and every function is exercisable in a unit test with no setup.
package certs

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// KeyAlgo names a keypair shape. ECDSA P-256 is the default for everything
// Daffa generates: small keys, fast handshakes, universal support. The RSA
// options exist for parity with material imported from internal-setup (the
// existing CA is RSA-4096, its leaf RSA-2048), not because new certs should
// use them.
type KeyAlgo string

const (
	ECDSAP256 KeyAlgo = "ecdsa-p256"
	RSA2048   KeyAlgo = "rsa-2048"
	RSA4096   KeyAlgo = "rsa-4096"
)

// backdate absorbs clock skew: a cert verified seconds after issuance by a
// machine whose clock runs slightly behind the box must not be "not yet valid".
const backdate = 5 * time.Minute

// Usages name the extended key usages a leaf is signed with, space-separated in
// the order below ("server", "client" or "server client") — the same encoding
// the store uses for SANs. The distinction is load-bearing for mTLS: Go's TLS
// stack refuses a client certificate without the clientAuth EKU, so a cert that
// silently lost it on renewal would be an outage. Callers therefore pass the
// STORED usages into every signing path rather than deriving them per call.
const (
	UsageServer = "server"
	UsageClient = "client"
)

// NormalizeUsages validates and canonicalizes a usages list: only server and
// client exist, duplicates collapse, order is fixed so string comparison works.
// Empty input means the historical default, server.
func NormalizeUsages(in []string) (string, error) {
	server, client := false, false
	for _, u := range in {
		switch strings.ToLower(strings.TrimSpace(u)) {
		case "":
		case UsageServer:
			server = true
		case UsageClient:
			client = true
		default:
			return "", fmt.Errorf("certs: unknown usage %q — a certificate is used as %q, %q or both", u, UsageServer, UsageClient)
		}
	}
	if !server && !client {
		server = true
	}
	return joinUsages(server, client), nil
}

// UsagesOf derives the usages string from an existing certificate's EKUs — for
// uploaded certs, where the column is a fact about the PEM, not a setting. A
// cert with no EKU extension is unrestricted, which is both usages.
func UsagesOf(cert *x509.Certificate) string {
	if len(cert.ExtKeyUsage) == 0 && len(cert.UnknownExtKeyUsage) == 0 {
		return joinUsages(true, true)
	}
	server, client := false, false
	for _, eku := range cert.ExtKeyUsage {
		switch eku {
		case x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageAny:
			server = true
			if eku == x509.ExtKeyUsageAny {
				client = true
			}
		case x509.ExtKeyUsageClientAuth:
			client = true
		}
	}
	return joinUsages(server, client)
}

func joinUsages(server, client bool) string {
	var out []string
	if server {
		out = append(out, UsageServer)
	}
	if client {
		out = append(out, UsageClient)
	}
	return strings.Join(out, " ")
}

// ekusFor maps a usages string onto the extension. Unknown words map to
// nothing on purpose — validation happened at NormalizeUsages; an empty result
// falls back to serverAuth so a bad value fails toward the historical default
// rather than toward an unrestricted cert.
func ekusFor(usages string) []x509.ExtKeyUsage {
	var out []x509.ExtKeyUsage
	for _, u := range strings.Fields(usages) {
		switch u {
		case UsageServer:
			out = append(out, x509.ExtKeyUsageServerAuth)
		case UsageClient:
			out = append(out, x509.ExtKeyUsageClientAuth)
		}
	}
	if len(out) == 0 {
		out = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	return out
}

// GenerateKey makes a fresh private key of the given shape.
func GenerateKey(algo KeyAlgo) (crypto.Signer, error) {
	switch algo {
	case ECDSAP256:
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case RSA2048:
		return rsa.GenerateKey(rand.Reader, 2048)
	case RSA4096:
		return rsa.GenerateKey(rand.Reader, 4096)
	default:
		return nil, fmt.Errorf("certs: unknown key algorithm %q", algo)
	}
}

// CreateCA generates a self-signed root: CA:TRUE critical, keyCertSign+cRLSign
// — the same extensions internal-ca.sh sets, so a created CA and an imported
// one are indistinguishable downstream. No path length constraint, matching
// the existing CA; the chain is flat (root signs leaves directly) by
// convention, not enforcement.
func CreateCA(commonName, org string, algo KeyAlgo, days int) (certPEM, keyPEM string, err error) {
	if strings.TrimSpace(commonName) == "" {
		return "", "", fmt.Errorf("certs: a CA needs a common name")
	}
	if days <= 0 {
		return "", "", fmt.Errorf("certs: validity must be at least a day")
	}
	key, err := GenerateKey(algo)
	if err != nil {
		return "", "", err
	}
	sn, err := serial()
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	subject := pkix.Name{CommonName: commonName}
	if org != "" {
		subject.Organization = []string{org}
	}
	tpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               subject,
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.AddDate(0, 0, days),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, key.Public(), key)
	if err != nil {
		return "", "", fmt.Errorf("certs: creating CA certificate: %w", err)
	}
	keyPEM, err = encodeKey(key)
	if err != nil {
		return "", "", err
	}
	return encodeCert(der), keyPEM, nil
}

// Issue signs a fresh leaf for the given SANs. The first SAN is the CN. The
// usages string decides the EKUs — "server" is what the Traefik leaf carries,
// "server client" (or "client") is an mTLS identity.
func Issue(caCertPEM, caKeyPEM string, sans []string, algo KeyAlgo, days int, usages string) (certPEM, keyPEM string, err error) {
	key, err := GenerateKey(algo)
	if err != nil {
		return "", "", err
	}
	if len(sans) == 0 {
		return "", "", fmt.Errorf("certs: a certificate needs at least one SAN")
	}
	certPEM, err = sign(caCertPEM, caKeyPEM, key.Public(), sans[0], sans, days, usages)
	if err != nil {
		return "", "", err
	}
	keyPEM, err = encodeKey(key)
	if err != nil {
		return "", "", err
	}
	return certPEM, keyPEM, nil
}

// Renew re-signs an existing leaf: same key, same CN, same SANs, fresh
// validity — the renew-internal-certs.sh behavior. Reusing the key is what
// makes renewal invisible to consumers; rotating the key is a separate,
// deliberate action (issue with the same SANs). Usages come from the caller
// (the stored row), not from the old PEM — the row is the source of truth an
// operator edits, and the next renewal is how an edit takes effect.
func Renew(caCertPEM, caKeyPEM, oldCertPEM, keyPEM string, days int, usages string) (string, error) {
	old, err := ParseCert(oldCertPEM)
	if err != nil {
		return "", err
	}
	key, err := ParseKey(keyPEM)
	if err != nil {
		return "", err
	}
	return sign(caCertPEM, caKeyPEM, key.Public(), old.Subject.CommonName, SANList(old), days, usages)
}

// Reissue signs a leaf for NEW SANs with an EXISTING key — what editing a
// certificate's SANs does. The key survives so the delivered key file never
// changes; only the certificate beside it does.
func Reissue(caCertPEM, caKeyPEM, keyPEM string, sans []string, days int, usages string) (string, error) {
	key, err := ParseKey(keyPEM)
	if err != nil {
		return "", err
	}
	if len(sans) == 0 {
		return "", fmt.Errorf("certs: a certificate needs at least one SAN")
	}
	return sign(caCertPEM, caKeyPEM, key.Public(), sans[0], sans, days, usages)
}

func sign(caCertPEM, caKeyPEM string, pub crypto.PublicKey, cn string, sans []string, days int, usages string) (string, error) {
	if days <= 0 {
		return "", fmt.Errorf("certs: validity must be at least a day")
	}
	ca, err := ParseCert(caCertPEM)
	if err != nil {
		return "", fmt.Errorf("certs: bad CA certificate: %w", err)
	}
	caKey, err := ParseKey(caKeyPEM)
	if err != nil {
		return "", fmt.Errorf("certs: bad CA key: %w", err)
	}
	if err := pairMatches(ca.PublicKey, caKey); err != nil {
		return "", fmt.Errorf("certs: the CA key does not belong to the CA certificate")
	}
	dns, ips := splitSANs(sans)
	if len(dns) == 0 && len(ips) == 0 {
		return "", fmt.Errorf("certs: a certificate needs at least one SAN")
	}
	sn, err := serial()
	if err != nil {
		return "", err
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.AddDate(0, 0, days),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  ekusFor(usages),
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	// Never outlive the signer: a leaf that expires after its CA verifies as
	// broken the day the CA lapses, and nothing would say why.
	if tpl.NotAfter.After(ca.NotAfter) {
		tpl.NotAfter = ca.NotAfter
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, pub, caKey)
	if err != nil {
		return "", fmt.Errorf("certs: signing: %w", err)
	}
	return encodeCert(der), nil
}

// Verify checks that certPEM chains to caCertPEM and is valid right now — the
// openssl-verify-before-install step the renewal cron does, kept for the same
// reason: never replace a working cert with a broken one.
func Verify(certPEM, caCertPEM string) error {
	cert, err := ParseCert(certPEM)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	for _, ca := range mustParseAll(caCertPEM) {
		roots.AddCert(ca)
	}
	_, err = cert.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	if err != nil {
		return fmt.Errorf("certs: certificate does not verify against its CA: %w", err)
	}
	return nil
}

// ParseCert returns the first CERTIFICATE block.
func ParseCert(pemText string) (*x509.Certificate, error) {
	all, err := ParseCerts(pemText)
	if err != nil {
		return nil, err
	}
	return all[0], nil
}

// ParseCerts returns every CERTIFICATE block, in order (for chains and bundles).
func ParseCerts(pemText string) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := []byte(pemText)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("certs: not a valid certificate: %w", err)
		}
		out = append(out, cert)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("certs: no certificate found (expected a PEM CERTIFICATE block)")
	}
	return out, nil
}

// ParseKey accepts a private key in any of the encodings openssl has ever
// liked: PKCS#8 ("PRIVATE KEY"), PKCS#1 ("RSA PRIVATE KEY"), SEC1 ("EC
// PRIVATE KEY"). Uploads come from whatever tooling produced them.
func ParseKey(pemText string) (crypto.Signer, error) {
	rest := []byte(pemText)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, fmt.Errorf("certs: no private key found (expected a PEM PRIVATE KEY block)")
		}
		switch block.Type {
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("certs: not a valid private key: %w", err)
			}
			signer, ok := k.(crypto.Signer)
			if !ok {
				return nil, fmt.Errorf("certs: unsupported private key type %T", k)
			}
			return signer, nil
		case "RSA PRIVATE KEY":
			k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("certs: not a valid RSA private key: %w", err)
			}
			return k, nil
		case "EC PRIVATE KEY":
			k, err := x509.ParseECPrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("certs: not a valid EC private key: %w", err)
			}
			return k, nil
		case "ENCRYPTED PRIVATE KEY":
			return nil, fmt.Errorf("certs: the key is passphrase-protected — decrypt it first (openssl pkey -in key.pem -out plain.pem)")
		}
	}
}

// CheckPair confirms the private key belongs to the certificate — the check
// that turns "Traefik silently refuses the pair at 3am" into a 400 at upload.
func CheckPair(certPEM, keyPEM string) error {
	cert, err := ParseCert(certPEM)
	if err != nil {
		return err
	}
	key, err := ParseKey(keyPEM)
	if err != nil {
		return err
	}
	return pairMatches(cert.PublicKey, key)
}

func pairMatches(certPub crypto.PublicKey, key crypto.Signer) error {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	pub, ok := key.Public().(equaler)
	if !ok || !pub.Equal(certPub) {
		return fmt.Errorf("certs: the private key does not match the certificate")
	}
	return nil
}

// ValidateCAUpload vets an uploaded CA. The key may be empty: a cert-only
// upload is a trust anchor Daffa can bundle and deliver but not sign with.
func ValidateCAUpload(certPEM, keyPEM string) error {
	cert, err := ParseCert(certPEM)
	if err != nil {
		return err
	}
	if !cert.BasicConstraintsValid || !cert.IsCA {
		return fmt.Errorf("certs: this certificate is not a CA (basicConstraints CA:TRUE is missing)")
	}
	if cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return fmt.Errorf("certs: this CA certificate is not allowed to sign (keyUsage lacks keyCertSign)")
	}
	if time.Now().After(cert.NotAfter) {
		return fmt.Errorf("certs: this CA expired %s", cert.NotAfter.Format("2006-01-02"))
	}
	if keyPEM != "" {
		return CheckPair(certPEM, keyPEM)
	}
	return nil
}

// ValidateLeafUpload vets an uploaded certificate + optional chain + key.
func ValidateLeafUpload(certPEM, chainPEM, keyPEM string) error {
	cert, err := ParseCert(certPEM)
	if err != nil {
		return err
	}
	if cert.BasicConstraintsValid && cert.IsCA {
		return fmt.Errorf("certs: this is a CA certificate — upload it as an authority instead")
	}
	if time.Now().After(cert.NotAfter) {
		return fmt.Errorf("certs: this certificate expired %s", cert.NotAfter.Format("2006-01-02"))
	}
	if chainPEM != "" {
		if _, err := ParseCerts(chainPEM); err != nil {
			return fmt.Errorf("certs: bad chain: %w", err)
		}
	}
	return CheckPair(certPEM, keyPEM)
}

// SANList flattens a certificate's DNS and IP SANs back into the one list the
// store keeps and the UI edits.
func SANList(cert *x509.Certificate) []string {
	out := append([]string{}, cert.DNSNames...)
	for _, ip := range cert.IPAddresses {
		out = append(out, ip.String())
	}
	return out
}

// DescribeKey names a public key's shape in KeyAlgo vocabulary, falling back
// to something honest for imported material Daffa would not have generated.
func DescribeKey(pub crypto.PublicKey) string {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if k.Curve == elliptic.P256() {
			return string(ECDSAP256)
		}
		return "ecdsa-" + strings.ToLower(k.Curve.Params().Name)
	case *rsa.PublicKey:
		return fmt.Sprintf("rsa-%d", k.N.BitLen())
	default:
		return fmt.Sprintf("%T", pub)
	}
}

// Bundle concatenates trust anchors into one PEM file, re-encoded so the
// output is uniform regardless of how the inputs were pasted.
func Bundle(caPEMs ...string) (string, error) {
	var b strings.Builder
	for _, p := range caPEMs {
		if strings.TrimSpace(p) == "" {
			continue
		}
		certs, err := ParseCerts(p)
		if err != nil {
			return "", err
		}
		for _, c := range certs {
			b.WriteString(encodeCert(c.Raw))
		}
	}
	return b.String(), nil
}

func splitSANs(sans []string) (dns []string, ips []net.IP) {
	for _, s := range sans {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if ip := net.ParseIP(s); ip != nil {
			ips = append(ips, ip)
		} else {
			dns = append(dns, s)
		}
	}
	return dns, ips
}

func serial() (*big.Int, error) {
	// 128 random bits, the modern replacement for openssl's -CAcreateserial
	// counter file (which internal-ca.sh had to remember to delete on rotation).
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("certs: generating serial: %w", err)
	}
	return n, nil
}

func encodeCert(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func encodeKey(key crypto.Signer) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("certs: encoding key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

func mustParseAll(pemText string) []*x509.Certificate {
	all, err := ParseCerts(pemText)
	if err != nil {
		return nil
	}
	return all
}
