package certs

import (
	"bytes"
	"crypto/x509"
	"io"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

func TestCreateIssueVerify(t *testing.T) {
	caCert, caKey, err := CreateCA("Example Internal CA", "Example", ECDSAP256, 3650)
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}
	if err := ValidateCAUpload(caCert, caKey); err != nil {
		t.Fatalf("a created CA should validate as an upload: %v", err)
	}

	sans := []string{"app.example.com", "www.example.com", "10.0.0.5"}
	certPEM, keyPEM, err := Issue(caCert, caKey, sans, ECDSAP256, 398, UsageServer)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := Verify(certPEM, caCert); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := CheckPair(certPEM, keyPEM); err != nil {
		t.Fatalf("CheckPair: %v", err)
	}

	cert, err := ParseCert(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "app.example.com" {
		t.Errorf("CN = %q, want first SAN", cert.Subject.CommonName)
	}
	got := SANList(cert)
	if len(got) != 3 || got[0] != "app.example.com" || got[2] != "10.0.0.5" {
		t.Errorf("SANList = %v, want %v", got, sans)
	}
	if cert.IsCA {
		t.Error("a leaf must not be a CA")
	}
}

func TestRenewReusesKeyAndSANs(t *testing.T) {
	caCert, caKey, _ := CreateCA("Test CA", "", ECDSAP256, 3650)
	certPEM, keyPEM, _ := Issue(caCert, caKey, []string{"a.example", "b.example"}, RSA2048, 30, UsageServer)

	renewed, err := Renew(caCert, caKey, certPEM, keyPEM, 398, UsageServer)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if err := Verify(renewed, caCert); err != nil {
		t.Fatalf("renewed cert does not verify: %v", err)
	}
	// Same key: the old private key must still match the new cert.
	if err := CheckPair(renewed, keyPEM); err != nil {
		t.Fatalf("renewal must reuse the key: %v", err)
	}
	old, _ := ParseCert(certPEM)
	renewedCert, _ := ParseCert(renewed)
	if strings.Join(SANList(renewedCert), " ") != strings.Join(SANList(old), " ") {
		t.Errorf("renewal changed SANs: %v -> %v", SANList(old), SANList(renewedCert))
	}
	if renewedCert.Subject.CommonName != old.Subject.CommonName {
		t.Errorf("renewal changed CN: %q -> %q", old.Subject.CommonName, renewedCert.Subject.CommonName)
	}
	if !renewedCert.NotAfter.After(old.NotAfter) {
		t.Error("renewal did not extend validity")
	}
	if renewedCert.SerialNumber.Cmp(old.SerialNumber) == 0 {
		t.Error("renewal reused the serial number")
	}
}

func TestUsagesBecomeEKUs(t *testing.T) {
	caCert, caKey, _ := CreateCA("EKU CA", "", ECDSAP256, 3650)

	hasEKU := func(pemText string, want x509.ExtKeyUsage) bool {
		c, err := ParseCert(pemText)
		if err != nil {
			t.Fatal(err)
		}
		for _, eku := range c.ExtKeyUsage {
			if eku == want {
				return true
			}
		}
		return false
	}

	server, _, _ := Issue(caCert, caKey, []string{"s.example"}, ECDSAP256, 30, UsageServer)
	if !hasEKU(server, x509.ExtKeyUsageServerAuth) || hasEKU(server, x509.ExtKeyUsageClientAuth) {
		t.Error("a server cert must carry serverAuth and not clientAuth")
	}
	client, _, _ := Issue(caCert, caKey, []string{"c.example"}, ECDSAP256, 30, UsageClient)
	if hasEKU(client, x509.ExtKeyUsageServerAuth) || !hasEKU(client, x509.ExtKeyUsageClientAuth) {
		t.Error("a client cert must carry clientAuth and not serverAuth")
	}
	both, keyPEM, _ := Issue(caCert, caKey, []string{"b.example"}, ECDSAP256, 30, "server client")
	if !hasEKU(both, x509.ExtKeyUsageServerAuth) || !hasEKU(both, x509.ExtKeyUsageClientAuth) {
		t.Error("a server+client cert must carry both EKUs")
	}

	// Renewal signs with the usages it is HANDED — the stored row, not the old
	// PEM — so an edited usages set takes effect at the next renewal.
	widened, err := Renew(caCert, caKey, both, keyPEM, 30, UsageClient)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if hasEKU(widened, x509.ExtKeyUsageServerAuth) || !hasEKU(widened, x509.ExtKeyUsageClientAuth) {
		t.Error("renewal must sign with the handed usages, not the old cert's")
	}

	if got := UsagesOf(mustParse(t, both)); got != "server client" {
		t.Errorf("UsagesOf(both) = %q", got)
	}
	if got := UsagesOf(mustParse(t, server)); got != "server" {
		t.Errorf("UsagesOf(server) = %q", got)
	}

	if _, err := NormalizeUsages([]string{"Server", "client", "server"}); err != nil {
		t.Errorf("NormalizeUsages should accept case and duplicates: %v", err)
	}
	if got, _ := NormalizeUsages(nil); got != "server" {
		t.Errorf("NormalizeUsages(nil) = %q, want the server default", got)
	}
	if _, err := NormalizeUsages([]string{"codeSigning"}); err == nil {
		t.Error("NormalizeUsages must reject unknown usages")
	}
}

func mustParse(t *testing.T, pemText string) *x509.Certificate {
	t.Helper()
	c, err := ParseCert(pemText)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLeafNeverOutlivesCA(t *testing.T) {
	caCert, caKey, _ := CreateCA("Short CA", "", ECDSAP256, 10)
	certPEM, _, err := Issue(caCert, caKey, []string{"x.example"}, ECDSAP256, 398, UsageServer)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := ParseCert(certPEM)
	ca, _ := ParseCert(caCert)
	if cert.NotAfter.After(ca.NotAfter) {
		t.Errorf("leaf NotAfter %v exceeds CA NotAfter %v", cert.NotAfter, ca.NotAfter)
	}
}

func TestVerifyRejectsWrongCA(t *testing.T) {
	ca1, key1, _ := CreateCA("CA One", "", ECDSAP256, 3650)
	ca2, _, _ := CreateCA("CA Two", "", ECDSAP256, 3650)
	certPEM, _, _ := Issue(ca1, key1, []string{"x.example"}, ECDSAP256, 30, UsageServer)
	if err := Verify(certPEM, ca2); err == nil {
		t.Fatal("a leaf must not verify against a CA that did not sign it")
	}
	// But it verifies against a bundle containing both — the rotation overlap case.
	bundle, err := Bundle(ca2, ca1)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(certPEM, bundle); err != nil {
		t.Fatalf("a leaf must verify against a bundle containing its CA: %v", err)
	}
}

func TestCheckPairMismatch(t *testing.T) {
	caCert, caKey, _ := CreateCA("Test CA", "", ECDSAP256, 3650)
	certPEM, _, _ := Issue(caCert, caKey, []string{"x.example"}, ECDSAP256, 30, UsageServer)
	_, otherKey, _ := Issue(caCert, caKey, []string{"y.example"}, ECDSAP256, 30, UsageServer)
	if err := CheckPair(certPEM, otherKey); err == nil {
		t.Fatal("mismatched key must be rejected")
	}
}

func TestUploadValidationTellsCertsApart(t *testing.T) {
	caCert, caKey, _ := CreateCA("Test CA", "", RSA2048, 3650)
	certPEM, keyPEM, _ := Issue(caCert, caKey, []string{"x.example"}, ECDSAP256, 30, UsageServer)

	if err := ValidateCAUpload(certPEM, keyPEM); err == nil {
		t.Error("a leaf must not validate as a CA")
	}
	if err := ValidateLeafUpload(caCert, "", caKey); err == nil {
		t.Error("a CA must not validate as a leaf")
	}
	if err := ValidateCAUpload(caCert, ""); err != nil {
		t.Errorf("a cert-only CA upload (trust anchor) must be allowed: %v", err)
	}
	if err := ValidateLeafUpload(certPEM, caCert, keyPEM); err != nil {
		t.Errorf("leaf with chain: %v", err)
	}
}

func TestParseKeyFormats(t *testing.T) {
	// PKCS#8 is what encodeKey writes; PKCS#1/SEC1 arrive via upload. Round-trip
	// through Issue for PKCS#8, and hand-build the others from the same keys.
	caCert, caKey, _ := CreateCA("Test CA", "", ECDSAP256, 3650)
	_, keyPEM, _ := Issue(caCert, caKey, []string{"x.example"}, RSA2048, 30, UsageServer)
	if _, err := ParseKey(keyPEM); err != nil {
		t.Fatalf("PKCS#8: %v", err)
	}
	if _, err := ParseKey("not a key"); err == nil {
		t.Fatal("garbage must not parse")
	}
}

func TestDescribeKey(t *testing.T) {
	for algo, want := range map[KeyAlgo]string{ECDSAP256: "ecdsa-p256", RSA2048: "rsa-2048"} {
		key, err := GenerateKey(algo)
		if err != nil {
			t.Fatal(err)
		}
		if got := DescribeKey(key.Public()); got != want {
			t.Errorf("DescribeKey(%s) = %q, want %q", algo, got, want)
		}
	}
}

func TestBundleNormalizes(t *testing.T) {
	ca1, _, _ := CreateCA("CA One", "", ECDSAP256, 3650)
	ca2, _, _ := CreateCA("CA Two", "", ECDSAP256, 3650)
	bundle, err := Bundle(ca1, "", "  \n"+ca2)
	if err != nil {
		t.Fatal(err)
	}
	all, err := ParseCerts(bundle)
	if err != nil || len(all) != 2 {
		t.Fatalf("bundle should hold 2 certs, got %d (err %v)", len(all), err)
	}
}

func TestGenerateAgeKeyRoundTrip(t *testing.T) {
	recipient, identityFile, err := GenerateAgeKey(time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(recipient, "age1") {
		t.Fatalf("recipient %q", recipient)
	}
	if !strings.Contains(identityFile, "# public key: "+recipient) {
		t.Error("identity file must carry the public key comment (age-keygen format)")
	}

	// The recipient must be able to encrypt what the identity decrypts.
	rec, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, rec)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "backup bytes")
	w.Close()

	var id age.Identity
	for _, line := range strings.Split(identityFile, "\n") {
		if strings.HasPrefix(line, "AGE-SECRET-KEY-") {
			id, err = age.ParseX25519Identity(line)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if id == nil {
		t.Fatal("identity file holds no AGE-SECRET-KEY line")
	}
	r, err := age.Decrypt(&buf, id)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	if string(got) != "backup bytes" {
		t.Errorf("round trip = %q", got)
	}
}

func TestParseAgeRecipient(t *testing.T) {
	recipient, identityFile, _ := GenerateAgeKey(time.Now())
	if got, err := ParseAgeRecipient("  " + recipient + "\n"); err != nil || got != recipient {
		t.Errorf("valid recipient rejected: %v", err)
	}
	for _, line := range strings.Split(identityFile, "\n") {
		if strings.HasPrefix(line, "AGE-SECRET-KEY-") {
			if _, err := ParseAgeRecipient(line); err == nil || !strings.Contains(err.Error(), "PRIVATE") {
				t.Error("a private key must be rejected with an error that says so")
			}
		}
	}
	if _, err := ParseAgeRecipient("age1notakey"); err == nil {
		t.Error("garbage must be rejected")
	}
}
