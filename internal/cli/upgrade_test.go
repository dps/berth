package cli

import (
	"strings"
	"testing"
)

func TestVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"v0.2.0", "0.2.0", 0, true},
		{"v0.10.0", "v0.9.3", 1, true},
		{"0.1.9", "v0.2.0", -1, true},
		{"HEAD-abc1234", "v0.2.0", 0, false},
		{"v0.2.0-3-gabc1234", "v0.2.0", 0, false},
	} {
		if cmp, ok := compareVersions(c.a, c.b); cmp != c.cmp || ok != c.ok {
			t.Errorf("compare(%s, %s) = %d %v", c.a, c.b, cmp, ok)
		}
	}
	if w := versionSkew("v0.2.0", "0.2.0"); w != "" {
		t.Errorf("same version warned: %s", w)
	}
	if w := versionSkew("", "0.2.0"); w != "" {
		t.Errorf("local use warned: %s", w)
	}
	if w := versionSkew("v0.3.0", "0.2.0"); !strings.Contains(w, "older than your CLI") {
		t.Errorf("old Mac: %q", w)
	}
	if w := versionSkew("v0.1.0", "0.2.0"); !strings.Contains(w, "your CLI (v0.1.0) is older") {
		t.Errorf("old CLI: %q", w)
	}
	if w := versionSkew("HEAD-aaa", "HEAD-bbb"); !strings.Contains(w, "different builds") {
		t.Errorf("HEAD builds: %q", w)
	}
}
