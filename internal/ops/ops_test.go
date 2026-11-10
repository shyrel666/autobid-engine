package ops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// backupFixture writes a valid backup without PostgreSQL: Verify only reads files and the manifest.
func backupFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(filepath.Join(dir, "exports", "t1"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("database.dump", "dump")
	write("exports/t1/a.docx", "docx")
	dump, err := HashFile(dir, "database.dump")
	if err != nil {
		t.Fatal(err)
	}
	file, err := HashFile(dir, "exports/t1/a.docx")
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Format: 1, CreatedAt: time.Now(), Schema: "abc", Dump: dump, Files: []File{file}, KeyNotice: KeyReminder}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	write("manifest.json", string(b))
	return dir
}

func TestVerifyAcceptsAnIntactBackup(t *testing.T) {
	if _, err := Verify(backupFixture(t)); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsDamagedBackups(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"changed file": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "exports", "t1", "a.docx"), []byte("DOCX"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"missing file": func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "exports", "t1", "a.docx")); err != nil {
				t.Fatal(err)
			}
		},
		"file not in the manifest": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "exports", "extra.bin"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"path escaping the backup": func(t *testing.T, dir string) {
			p := filepath.Join(dir, "manifest.json")
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(strings.ReplaceAll(string(b), "exports/t1/a.docx", "exports/../../a.docx")), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			dir := backupFixture(t)
			damage(t, dir)
			if _, err := Verify(dir); err == nil {
				t.Fatal("Verify accepted a damaged backup")
			}
		})
	}
}

func TestRestoreTargetsMustBeEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := emptyDir(dir); err != nil {
		t.Fatalf("empty directory refused: %v", err)
	}
	if err := emptyDir(filepath.Join(dir, "missing")); err != nil {
		t.Fatalf("missing directory refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := emptyDir(dir); err == nil {
		t.Fatal("non-empty directory accepted")
	}
}

func TestErrorsNeverContainConnectionDetails(t *testing.T) {
	const secret = "s3cret-password"
	_, err := ParseDatabase("postgres://user:" + secret + "@127.0.0.1:notaport/x")
	if err == nil {
		t.Fatal("invalid configuration accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestProductionDatabaseIsAllowed(t *testing.T) {
	if _, err := ParseDatabase("postgres://u:p@127.0.0.1:5432/autobid"); err != nil {
		t.Fatalf("the operator tool must be able to back up the real database: %v", err)
	}
}
