// Package sshx holds Daffa's SSH primitives: key generation and parsing today, and the
// dial-out transport a later phase adds (docs/clusters.md §2). Everything here is about
// Daffa reaching a machine over SSH — never about accepting an inbound connection.
package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ErrPassphraseRequired is returned when a private key is encrypted but no passphrase was
// supplied. It is a distinct error so the API can ask for the passphrase specifically rather
// than reporting a generic parse failure.
var ErrPassphraseRequired = errors.New("this private key is encrypted; a passphrase is required")

// KeyMaterial is a keypair split the way the store keeps it: the private half as an OpenSSH
// PEM block (which gets sealed), and the public half as one authorized_keys line plus its
// fingerprint (which stay plaintext).
type KeyMaterial struct {
	Algo          string // ed25519 | rsa | ecdsa
	PrivatePEM    string // OpenSSH private key, PEM-encoded (empty for PublicFromPrivate)
	AuthorizedKey string // one authorized_keys line, e.g. "ssh-ed25519 AAAA… comment"
	Fingerprint   string // SHA256:…
}

// Generate mints a fresh keypair. algo is "ed25519" (the default, empty means this) or "rsa"
// (4096-bit). comment is appended to the public line so a human — and the target's
// authorized_keys — can tell whose key it is.
func Generate(algo, comment string) (*KeyMaterial, error) {
	switch algo {
	case "", "ed25519":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("sshx: generating ed25519 key: %w", err)
		}
		return marshalPair("ed25519", priv, pub, comment)
	case "rsa":
		priv, err := rsa.GenerateKey(rand.Reader, 4096)
		if err != nil {
			return nil, fmt.Errorf("sshx: generating rsa key: %w", err)
		}
		return marshalPair("rsa", priv, priv.Public(), comment)
	default:
		return nil, fmt.Errorf("sshx: unknown key algorithm %q (use ed25519 or rsa)", algo)
	}
}

// marshalPair turns a freshly generated keypair into stored material: the private key sealed
// as OpenSSH PEM, the public key as an authorized_keys line with fingerprint.
func marshalPair(algo string, priv any, pub any, comment string) (*KeyMaterial, error) {
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, fmt.Errorf("sshx: marshalling private key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("sshx: deriving public key: %w", err)
	}
	return &KeyMaterial{
		Algo:          algo,
		PrivatePEM:    string(pem.EncodeToMemory(block)),
		AuthorizedKey: authorizedLine(sshPub, comment),
		Fingerprint:   ssh.FingerprintSHA256(sshPub),
	}, nil
}

// PublicFromPrivate parses an imported private key and derives its public half — so an
// operator imports the private key alone and Daffa recomputes the public line and fingerprint
// rather than trusting a pasted public half to match. PrivatePEM is left empty; the caller
// already holds the PEM it passed in.
func PublicFromPrivate(privatePEM, passphrase, comment string) (*KeyMaterial, error) {
	var signer ssh.Signer
	var err error
	if passphrase != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(privatePEM), []byte(passphrase))
	} else {
		signer, err = ssh.ParsePrivateKey([]byte(privatePEM))
		if _, ok := err.(*ssh.PassphraseMissingError); ok {
			return nil, ErrPassphraseRequired
		}
	}
	if err != nil {
		return nil, fmt.Errorf("sshx: parsing private key: %w", err)
	}

	pub := signer.PublicKey()
	return &KeyMaterial{
		Algo:          algoFromType(pub.Type()),
		AuthorizedKey: authorizedLine(pub, comment),
		Fingerprint:   ssh.FingerprintSHA256(pub),
	}, nil
}

// authorizedLine renders one authorized_keys line and appends the comment. MarshalAuthorizedKey
// emits "<type> <base64>\n" with no comment, so we trim the newline and add the comment back.
func authorizedLine(pub ssh.PublicKey, comment string) string {
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	if comment = strings.TrimSpace(comment); comment != "" {
		line += " " + comment
	}
	return line
}

// algoFromType maps an SSH public-key type ("ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256")
// to the short label the store keeps. ECDSA keeps its family name; the rest drop the "ssh-".
func algoFromType(t string) string {
	switch {
	case t == "ssh-ed25519":
		return "ed25519"
	case t == "ssh-rsa":
		return "rsa"
	case strings.HasPrefix(t, "ecdsa-"):
		return "ecdsa"
	default:
		return strings.TrimPrefix(t, "ssh-")
	}
}
