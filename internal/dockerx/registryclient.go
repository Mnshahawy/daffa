package dockerx

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"time"
)

// registryClient builds the HTTP client Daffa uses to reach a registry, verifying the registry's
// certificate against roots. nil means the system roots — the unchanged public-registry path.
//
// The API layer passes system roots ∪ Daffa's own managed CAs, so a registry fronted by a
// Daffa-issued cert (the common internal case) verifies with zero per-registry config. There is
// deliberately NO skip-verify and NO operator-pasted CA: Daffa trusts the public web and the CAs
// it manages itself, nothing typed into a form. A fresh client per call is fine — registry
// reach-out is infrequent and never on a hot path.
func registryClient(roots *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout: 12 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots},
		},
	}
}
