package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Secrets held at rest (registry passwords, stack env values, S3 keys) are sealed
// with AES-256-GCM under the master key. Losing the key costs you the secrets, not
// the rest of the data — every sealed column is re-enterable.

const keyLen = 32

func NewMasterKey() ([]byte, error) {
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("config: generating master key: %w", err)
	}
	return key, nil
}

func encodeKey(key []byte) string { return base64.StdEncoding.EncodeToString(key) }

func parseKey(raw []byte) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("not valid base64: %w", err)
	}
	if len(key) != keyLen {
		return nil, fmt.Errorf("want %d bytes, got %d", keyLen, len(key))
	}
	return key, nil
}

// Sealer seals and opens secret values with the master key.
type Sealer struct{ aead cipher.AEAD }

func NewSealer(key []byte) (*Sealer, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("config: sealer: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("config: sealer: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// Seal returns a base64 string safe to store in a TEXT column.
func (s *Sealer) Seal(plaintext string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("config: seal: %w", err)
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *Sealer) Open(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("config: open: %w", err)
	}
	if len(raw) < s.aead.NonceSize() {
		return "", errors.New("config: open: ciphertext too short")
	}
	nonce, ct := raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():]
	plain, err := s.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		// Wrong master key, or the value was tampered with.
		return "", fmt.Errorf("config: open: %w", err)
	}
	return string(plain), nil
}
