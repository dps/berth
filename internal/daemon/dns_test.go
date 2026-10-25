package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

func TestSRVResponder(t *testing.T) {
	d, _, repo := newTestDaemon(t)
	params, _ := json.Marshal(CreateParams{Name: "web", Repo: repo})
	if _, err := d.hCreate(d.ctx, params, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	pc, err := listenDNS("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go d.serveDNS(pc)
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", pc.LocalAddr().String())
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, srvs, err := r.LookupSRV(ctx, "web", "tcp", SRVDomain)
	if err != nil {
		t.Fatal(err)
	}
	if len(srvs) != 1 || int(srvs[0].Port) != d.Store.CurrentRelease("web").HostPort || srvs[0].Target != "localhost." {
		t.Fatalf("srv %+v, want port %d", srvs[0], d.Store.CurrentRelease("web").HostPort)
	}
	if _, _, err := r.LookupSRV(ctx, "nope", "tcp", SRVDomain); err == nil {
		t.Fatal("unknown app should be NXDOMAIN")
	}
}
