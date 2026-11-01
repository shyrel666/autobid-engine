package secrets

import (
	"path/filepath"
	"testing"
)

func TestVaultRestartAndWrongKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	v := &Vault{Path: path}
	c, err := v.Encrypt("test-only-credential")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := (&Vault{Path: path}).Decrypt(c)
	if err != nil || plain != "test-only-credential" {
		t.Fatal("restart did not recover credential", err)
	}
	wrong := &Vault{Path: filepath.Join(t.TempDir(), "other.key")}
	_, _ = wrong.Encrypt("other")
	if _, err := wrong.Decrypt(c); err == nil {
		t.Fatal("wrong master key accepted")
	}
	if _, err := (&Vault{Path: filepath.Join(t.TempDir(), "missing")}).Decrypt(c); err == nil {
		t.Fatal("missing key silently recreated")
	}
}
