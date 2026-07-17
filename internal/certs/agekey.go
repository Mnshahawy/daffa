package certs

import (
	"fmt"
	"strings"
	"time"

	"filippo.io/age"
)

// GenerateAgeKey creates an X25519 age identity IN MEMORY and returns it
// exactly once. The caller hands identityFile to the user as a download and
// stores only the recipient — the private key is never written to the
// database, to disk, or to a log. That is the entire security design of
// encryption keys: the box can encrypt backups it cannot read.
//
// identityFile is the format age-keygen writes, so every age tool and the
// daffa CLI's --identity flag accept it unmodified.
func GenerateAgeKey(now time.Time) (recipient, identityFile string, err error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", fmt.Errorf("certs: generating age key: %w", err)
	}
	recipient = id.Recipient().String()
	identityFile = fmt.Sprintf("# created: %s\n# public key: %s\n%s\n",
		now.Format(time.RFC3339), recipient, id.String())
	return recipient, identityFile, nil
}

// ParseAgeRecipient validates a pasted public key. It refuses anything that
// smells like a private key before attempting to parse, so the error names
// the actual mistake instead of "invalid recipient".
func ParseAgeRecipient(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(strings.ToUpper(s), "AGE-SECRET-KEY-") {
		return "", fmt.Errorf("certs: that is an age PRIVATE key — it must never be sent to the server; paste the public key (it starts with age1)")
	}
	r, err := age.ParseX25519Recipient(s)
	if err != nil {
		return "", fmt.Errorf("certs: not a valid age public key (it should start with age1)")
	}
	return r.String(), nil
}
