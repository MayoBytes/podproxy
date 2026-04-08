package proxy

import (
	"net/http"
	"testing"
)

func TestIsNewPlay(t *testing.T) {
	cases := []struct {
		name  string
		range_ string
		want  bool
	}{
		{"no Range header", "", true},
		{"open-ended from zero", "bytes=0-", true},
		{"single-byte probe from zero", "bytes=0-1", true},
		{"large range from zero", "bytes=0-999", true},
		{"mid-episode seek", "bytes=500-999", false},
		{"seek from byte 1", "bytes=1-", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{Header: make(http.Header)}
			if tc.range_ != "" {
				r.Header.Set("Range", tc.range_)
			}
			if got := isNewPlay(r); got != tc.want {
				t.Errorf("isNewPlay(%q) = %v, want %v", tc.range_, got, tc.want)
			}
		})
	}
}
