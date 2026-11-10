package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryExportBlockerHasAPageAction guards the gap S7 found: new gates were added on the
// server while the export page still said "unknown blocker, contact the administrator".
func TestEveryExportBlockerHasAPageAction(t *testing.T) {
	read := func(parts ...string) string {
		b, err := os.ReadFile(filepath.Join(append([]string{".."}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	files, err := filepath.Glob(filepath.Join("..", "db", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	constRe := regexp.MustCompile(`(?m)^\s*Block[A-Z][A-Za-z]*\s*=\s*"([a-z_]+)"`)
	literalRe := regexp.MustCompile(`ExportBlocker\{Kind:\s*"([a-z_]+)"`)
	kinds := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range []*regexp.Regexp{constRe, literalRe} {
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				kinds[m[1]] = true
			}
		}
	}
	if len(kinds) < 20 {
		t.Fatalf("found only %d blocker kinds; the scan no longer matches db/", len(kinds))
	}
	links := read("web", "src", "features", "task-workspace", "blocker-links.ts")
	labels := read("web", "src", "lib", "format.ts")
	for kind := range kinds {
		if !strings.Contains(links, "'"+kind+"'") {
			t.Errorf("export blocker %q has no fix link in blocker-links.ts", kind)
		}
		if !strings.Contains(labels, kind+":") {
			t.Errorf("export blocker %q has no label in format.ts", kind)
		}
	}
}
