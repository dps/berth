package state

import (
	"fmt"
	"regexp"
	"strings"
)

// Values that end up in git argv, shell hooks, Caddy matchers or Cloudflare
// expressions are validated where they enter (CLI and RPC), so nothing
// downstream has to escape them.

var safeRefRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// ValidateRef accepts branch, tag and commit names made of letters, digits
// and . _ / -, following git's ref rules, and never starting with "-" (which
// git would parse as an option).
func ValidateRef(kind, ref string) error {
	bad := ref == "" || len(ref) > 200 || !safeRefRe.MatchString(ref) ||
		strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") ||
		strings.HasPrefix(ref, ".") || strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, ".lock") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "//")
	if bad {
		return fmt.Errorf("invalid %s %q: use letters, digits and . _ / - (git ref rules)", kind, ref)
	}
	return nil
}

// ValidateRepo accepts owner/name, a URL or a path, but never anything git
// could read as an option or that contains whitespace or control characters.
func ValidateRepo(repo string) error {
	if repo == "" {
		return nil
	}
	if strings.HasPrefix(repo, "-") || strings.ContainsAny(repo, " \t\r\n\x00'\"`$\\;|&<>") {
		return fmt.Errorf("invalid repo %q", repo)
	}
	return nil
}

var hostLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateHostname accepts a lowercase DNS name with at least two labels and
// no wildcards.
func ValidateHostname(h string) error {
	if len(h) > 253 {
		return fmt.Errorf("invalid hostname %q: too long", h)
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return fmt.Errorf("invalid hostname %q: needs a domain", h)
	}
	for _, l := range labels {
		if !hostLabelRe.MatchString(l) {
			return fmt.Errorf("invalid hostname %q (letters, digits and dashes per label; no wildcards)", h)
		}
	}
	return nil
}
