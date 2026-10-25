package launchd

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestPlistEscapesAndParses(t *testing.T) {
	body, err := Plist(Agent{Label: LabelBerthd, Args: []string{"/bin/berthd", "a&b<c>"}, Env: map[string]string{"BERTH_HOME": "/Users/x/.berth"}, Log: "/tmp/l.log"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "a&amp;b&lt;c&gt;") || !strings.Contains(body, "<key>KeepAlive</key>\n  <true/>") {
		t.Fatal(body)
	}
	d := xml.NewDecoder(strings.NewReader(body))
	d.Strict = false
	for {
		if _, err := d.Token(); err != nil {
			if err.Error() == "EOF" {
				return
			}
			t.Fatalf("invalid XML: %v", err)
		}
	}
}
