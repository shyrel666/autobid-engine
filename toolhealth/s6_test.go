package toolhealth

import (
	"strings"
	"testing"
)

func TestSanitizeModelHidesEndpointsAndPaths(t *testing.T) {
	got := SanitizeModel(`POST https://user:password@private-model.example/model?api_key=private-key failed; C:\private-host\model.bin; Bearer sk-private-token`)
	for _, private := range []string{"https://", "private-model", "private-key", "private-host", "sk-private", "password@"} {
		if strings.Contains(got, private) {
			t.Fatalf("private model diagnostic leaked: %s", got)
		}
	}
	if !strings.Contains(got, "<endpoint>") || !strings.Contains(got, "<path>") {
		t.Fatal(got)
	}
}
