package state

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Access levels for sign-in (see design.md, Sign-in).
const (
	LevelPublic   = "public"
	LevelSignedIn = "signed-in"
	LevelAllow    = "allow"
)

// Access is an app's sign-in policy: a default rule plus per-path rules.
// A nil *Access means the app has no policy and berthd isn't consulted.
type Access struct {
	Level string     `json:"level"`
	Allow []string   `json:"allow,omitempty"`
	Paths []PathRule `json:"paths,omitempty"`
}

// PathRule overrides the default for one path. A trailing * makes Path a
// prefix; otherwise it matches exactly.
type PathRule struct {
	Path  string   `json:"path"`
	Level string   `json:"level"`
	Allow []string `json:"allow,omitempty"`
}

// Rule is the effective rule for one request.
type Rule struct {
	Level string
	Allow []string
}

// For returns the rule for path: the longest matching path rule, else the
// default.
func (a *Access) For(path string) Rule {
	best, bestLen := Rule{Level: a.Level, Allow: a.Allow}, -1
	for _, p := range a.Paths {
		n := -1
		if prefix, ok := strings.CutSuffix(p.Path, "*"); ok {
			// "/compose/*" also covers "/compose".
			if strings.HasPrefix(path, prefix) || path+"/" == prefix {
				n = len(prefix)
			}
		} else if path == p.Path {
			n = len(p.Path) + 1 // an exact match beats a prefix of the same length
		}
		if n > bestLen {
			best, bestLen = Rule{Level: p.Level, Allow: p.Allow}, n
		}
	}
	return best
}

// SetPath adds or replaces the rule for path; with level "" it removes it.
func (a *Access) SetPath(path, level string, allow []string) {
	var out []PathRule
	for _, p := range a.Paths {
		if p.Path != path {
			out = append(out, p)
		}
	}
	if level != "" {
		out = append(out, PathRule{Path: path, Level: level, Allow: allow})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	a.Paths = out
}

func (a *Access) String() string {
	var b strings.Builder
	b.WriteString(ruleString(a.Level, a.Allow))
	for _, p := range a.Paths {
		fmt.Fprintf(&b, "; %s %s", p.Path, ruleString(p.Level, p.Allow))
	}
	return b.String()
}

func ruleString(level string, allow []string) string {
	if level == LevelAllow {
		return level + " " + strings.Join(allow, " ")
	}
	return level
}

func ValidateLevel(level string, allow []string) error {
	switch level {
	case LevelPublic, LevelSignedIn:
		if len(allow) > 0 {
			return fmt.Errorf("%s takes no list of people (use allow)", level)
		}
	case LevelAllow:
		if len(allow) == 0 {
			return fmt.Errorf("allow needs at least one email, @domain or @group")
		}
		for _, w := range allow {
			if err := ValidateAllowEntry(w); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown access level %q (public, signed-in, allow)", level)
	}
	return nil
}

var (
	emailRe     = regexp.MustCompile(`^[A-Za-z0-9._%+'-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)
	groupNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	accessPath  = regexp.MustCompile(`^/[A-Za-z0-9._~/%-]*\*?$`)
)

// ValidateAllowEntry accepts an email, @domain (contains a dot) or @group.
func ValidateAllowEntry(w string) error {
	if name, ok := strings.CutPrefix(w, "@"); ok {
		if strings.Contains(name, ".") {
			// @domain matches Google's hd claim, which only Workspace
			// accounts have: consumer Gmail would silently match nobody.
			switch strings.ToLower(name) {
			case "gmail.com", "googlemail.com":
				return fmt.Errorf("%s would match nobody: @domain means a Google Workspace domain. For any Google account use signed-in; otherwise list the addresses", w)
			}
			return ValidateHostname(strings.ToLower(name))
		}
		if !groupNameRe.MatchString(name) {
			return fmt.Errorf("invalid group %q: lowercase letters, digits and dashes", w)
		}
		return nil
	}
	if !emailRe.MatchString(w) {
		return fmt.Errorf("%q is not an email, @domain or @group", w)
	}
	return nil
}

func ValidateGroupName(name string) error {
	if !groupNameRe.MatchString(name) {
		return fmt.Errorf("invalid group name %q: lowercase letters, digits and dashes", name)
	}
	return nil
}

func ValidateAccessPath(p string) error {
	if len(p) > 200 || !accessPath.MatchString(p) || strings.Contains(p, "..") {
		return fmt.Errorf("invalid path %q: starts with /, optionally ends with *", p)
	}
	return nil
}

// AuthConfig is ~/.berth/auth/config.json: the sign-in provider and groups.
// The client secret lives in secrets/<provider>.client_secret.
type AuthConfig struct {
	Provider string              `json:"provider,omitempty"` // "google"
	ClientID string              `json:"client_id,omitempty"`
	Host     string              `json:"host,omitempty"` // auth.<domain>
	Groups   map[string][]string `json:"groups,omitempty"`
	// AdminHost serves Berth's admin page to the @owners group
	// (admin.<domain>); empty means off.
	AdminHost string `json:"admin_host,omitempty"`
}

func (s *Store) AuthConfig() (*AuthConfig, error) {
	c := &AuthConfig{}
	if err := readJSON(s.Path("auth", "config.json"), c); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if c.Groups == nil {
		c.Groups = map[string][]string{}
	}
	return c, nil
}

func (s *Store) SaveAuthConfig(c *AuthConfig) error {
	if err := os.MkdirAll(s.Path("auth"), 0o700); err != nil {
		return err
	}
	return writeJSON(s.Path("auth", "config.json"), c, 0o600)
}

// AllPublic reports whether every rule is public (nothing to hide from
// shared caches).
func (a *Access) AllPublic() bool {
	if a.Level != LevelPublic {
		return false
	}
	for _, p := range a.Paths {
		if p.Level != LevelPublic {
			return false
		}
	}
	return true
}
