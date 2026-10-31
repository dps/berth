package tools

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// The gateway runs on the Mac, where an app's URL could otherwise reach
// Caddy's admin API, berthd, Postgres or the LAN. Containers can't, so
// fetching a URL mustn't give them a way around that. The check runs at
// connect time on the address actually dialed, so it covers redirects and
// names that resolve differently the second time.

var blockedRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT, and Tailscale
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 reaches IPv4 addresses
	netip.MustParsePrefix("2001:db8::/32"),
}

var errPrivate = errors.New("that address isn't on the public internet")

// publicAddr reports whether a is on the public internet.
func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range blockedRanges {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func dialCheck(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !publicAddr(ap.Addr()) {
		return errPrivate
	}
	return nil
}

// publicClient fetches URLs given by apps. It has no proxy, which would
// dial for it, and sends no credentials.
func publicClient() *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: dialCheck}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           d.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if s := req.URL.Scheme; s != "http" && s != "https" {
				return fmt.Errorf("redirect to %s", s)
			}
			return nil
		},
	}
}
