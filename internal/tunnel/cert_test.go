package tunnel

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func certPEM(t *testing.T, c OriginCert) []byte {
	b, _ := json.Marshal(c)
	return []byte("-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----\n" +
		"-----BEGIN ARGO TUNNEL TOKEN-----\n" + base64.StdEncoding.EncodeToString(b) + "\n-----END ARGO TUNNEL TOKEN-----\n")
}

func TestParseOriginCert(t *testing.T) {
	c, err := ParseOriginCert(certPEM(t, OriginCert{ZoneID: "z", AccountID: "a", APIToken: "tok"}))
	if err != nil || c.ZoneID != "z" || c.AccountID != "a" || c.APIToken != "tok" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := ParseOriginCert([]byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")); err == nil {
		t.Fatal("expected error without a token block")
	}
}

func TestCertForHost(t *testing.T) {
	dir := t.TempDir()
	for _, z := range []string{"cronbon.com", "other.org", "sub.other.org"} {
		os.WriteFile(filepath.Join(dir, z+".pem"), []byte("x"), 0o600)
	}
	cases := map[string]string{
		"hello.cronbon.com": "cronbon.com",
		"cronbon.com":       "cronbon.com",
		"www.other.org":     "other.org",
		"a.sub.other.org":   "sub.other.org",
		"notcronbon.com":    "",
		"x.example.com":     "",
	}
	for host, want := range cases {
		if z, _ := CertForHost(dir, host); z != want {
			t.Errorf("%s → %q, want %q", host, z, want)
		}
	}
}
