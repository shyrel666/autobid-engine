package server

import (
	"net/http/httptest"
	"testing"
)

func TestOperatorHeader(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"encoded Chinese name from the page", "%E6%92%B0%E5%86%99%E4%BA%BA%E7%94%B2", "撰写人甲"},
		{"plain ASCII from an API client", "alice", "alice"},
		{"literal percent that does not decode stays as sent", "100%", "100%"},
		{"surrounding spaces", "%20bob%20", "bob"},
		{"absent", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if tc.raw != "" {
				r.Header.Set("X-Autobid-User", tc.raw)
			}
			if got := operator(r); got != tc.want {
				t.Fatalf("operator() = %q, want %q", got, tc.want)
			}
		})
	}
}
