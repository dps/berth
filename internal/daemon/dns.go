package daemon

import (
	"fmt"
	"log/slog"
	"net"
	"strings"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/dps/berth/internal/state"
)

// SRVDomain is the zone berthd answers for Caddy. Caddy's route for each app
// looks up _<app>._tcp.berth.internal (SRV) every second, and berthd answers
// with the current release's port. Switching a deploy's traffic is then a
// state change in berthd, not a Caddy config reload (which drops requests at
// the margins), and it is immediate in both slot directions.
const SRVDomain = "berth.internal"

// serveDNS answers SRV queries on the loopback UDP address until the
// listener is closed.
func (d *Daemon) serveDNS(pc net.PacketConn) {
	buf := make([]byte, 1500)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if resp, err := d.answerDNS(buf[:n]); err == nil {
			pc.WriteTo(resp, addr)
		}
	}
}

func (d *Daemon) answerDNS(req []byte) ([]byte, error) {
	var p dnsmessage.Parser
	hdr, err := p.Start(req)
	if err != nil {
		return nil, err
	}
	q, err := p.Question()
	if err != nil {
		return nil, err
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: hdr.ID, Response: true, Authoritative: true, RCode: dnsmessage.RCodeSuccess})
	b.EnableCompression()
	port, ok := d.srvPort(q.Name.String())
	if !ok {
		b = dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: hdr.ID, Response: true, Authoritative: true, RCode: dnsmessage.RCodeNameError})
	}
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(q); err != nil {
		return nil, err
	}
	if ok && q.Type == dnsmessage.TypeSRV {
		if err := b.StartAnswers(); err != nil {
			return nil, err
		}
		target := dnsmessage.MustNewName("localhost.")
		if err := b.SRVResource(
			dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 1},
			dnsmessage.SRVResource{Priority: 0, Weight: 1, Port: uint16(port), Target: target},
		); err != nil {
			return nil, err
		}
	}
	return b.Finish()
}

// srvPort maps _<app>._tcp.berth.internal. to the port of the app's
// current, running release.
func (d *Daemon) srvPort(name string) (int, bool) {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	rest, ok := strings.CutSuffix(name, "._tcp."+SRVDomain)
	if !ok || !strings.HasPrefix(rest, "_") {
		return 0, false
	}
	app := rest[1:]
	if state.ValidateAppName(app) != nil {
		return 0, false
	}
	rel := d.Store.CurrentRelease(app)
	if rel == nil || rel.Container == "" || rel.HostPort == 0 {
		return 0, false
	}
	return rel.HostPort, true
}

func listenDNS(addr string) (net.PacketConn, error) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("dns listener %s: %w", addr, err)
	}
	slog.Info("serving upstream SRV records", "addr", addr, "zone", SRVDomain)
	return pc, nil
}
