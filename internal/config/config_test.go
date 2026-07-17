package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The at-rest seal is only as strong as the file it lives in. A master key that arrives group- or
// world-readable (restored from a backup, copied by hand) must be refused, not loaded silently.
func TestLoadMasterKeyRefusesLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	key, err := NewMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(encodeKey(key)), 0o600); err != nil {
		t.Fatal(err)
	}

	// 0600 is accepted.
	if _, err := loadMasterKey(dir); err != nil {
		t.Fatalf("a 0600 master key was refused: %v", err)
	}

	// Group- or world-readable is refused, and the error names the fix.
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o666} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := loadMasterKey(dir); err == nil {
			t.Errorf("a %04o master key was loaded; want a refusal", mode)
		}
	}
}
