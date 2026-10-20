package tsapi

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
)

func pfx(ss ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

func TestMergeRoutes(t *testing.T) {
	cases := []struct {
		name                    string
		current, owned, wanted  []netip.Prefix
		wantNext, wantNextOwned []netip.Prefix
	}{
		{
			name:          "adds wanted and preserves foreign routes",
			current:       pfx("0.0.0.0/0", "::/0", "192.168.0.0/24"),
			wanted:        pfx("10.250.0.10/32"),
			wantNext:      pfx("0.0.0.0/0", "::/0", "192.168.0.0/24", "10.250.0.10/32"),
			wantNextOwned: pfx("10.250.0.10/32"),
		},
		{
			name:          "withdraws only owned routes",
			current:       pfx("0.0.0.0/0", "10.250.0.10/32", "10.250.0.11/32"),
			owned:         pfx("10.250.0.10/32"),
			wanted:        nil,
			wantNext:      pfx("0.0.0.0/0", "10.250.0.11/32"),
			wantNextOwned: nil,
		},
		{
			name:          "already-advertised wanted route stays unowned",
			current:       pfx("10.250.0.20/32"),
			wanted:        pfx("10.250.0.20/32"),
			wantNext:      pfx("10.250.0.20/32"),
			wantNextOwned: nil,
		},
		{
			name:          "owned route removed by someone else is re-added",
			current:       pfx("0.0.0.0/0"),
			owned:         pfx("10.250.0.10/32"),
			wanted:        pfx("10.250.0.10/32"),
			wantNext:      pfx("0.0.0.0/0", "10.250.0.10/32"),
			wantNextOwned: pfx("10.250.0.10/32"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next, owned := MergeRoutes(tc.current, tc.owned, tc.wanted)
			if !slices.Equal(next, tc.wantNext) || !slices.Equal(owned, tc.wantNextOwned) {
				t.Fatalf("MergeRoutes = %v, %v; want %v, %v", next, owned, tc.wantNext, tc.wantNextOwned)
			}
		})
	}
}

func status() *ipnstate.Status {
	routes := views.SliceOf(pfx("10.250.0.10/32"))
	other := views.SliceOf(pfx("10.99.0.0/16"))
	peer := func(host, dns string, ips ...string) *ipnstate.PeerStatus {
		p := &ipnstate.PeerStatus{HostName: host, DNSName: dns, Online: true}
		for _, ip := range ips {
			p.TailscaleIPs = append(p.TailscaleIPs, netip.MustParseAddr(ip))
		}
		return p
	}
	r2 := peer("router2", "router2.ts.net.", "100.90.0.9")
	r2.PrimaryRoutes = &other
	self := peer("gw", "gw.ts.net.", "100.64.0.1")
	self.PrimaryRoutes = &routes
	return &ipnstate.Status{
		Self: self,
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			key.NewNode().Public(): peer("Device-B", "device-b.ts.net.", "fd7a::1", "100.101.102.103"),
			key.NewNode().Public(): peer("web", "web.ts.net.", "100.101.0.1"),
			key.NewNode().Public(): peer("web", "web-1.ts.net.", "100.101.0.2"),
			key.NewNode().Public(): r2,
		},
	}
}

func TestResolvePeer(t *testing.T) {
	st := status()
	for _, spec := range []string{"device-b", "DEVICE-B", "device-b.ts.net", "device-b.ts.net.", "100.101.102.103"} {
		p, err := ResolvePeer(st, spec)
		if err != nil || p.IP != netip.MustParseAddr("100.101.102.103") || p.Name != "device-b.ts.net" {
			t.Errorf("ResolvePeer(%q) = %+v, %v", spec, p, err)
		}
	}
	if p, err := ResolvePeer(st, "web-1"); err != nil || p.IP != netip.MustParseAddr("100.101.0.2") {
		t.Errorf("ResolvePeer(web-1) = %+v, %v", p, err)
	}
	for spec, wantErr := range map[string]string{
		"web":        "ambiguous",
		"nope":       "no tailnet peer",
		"gw":         "itself",
		"100.64.0.1": "itself",
	} {
		if _, err := ResolvePeer(st, spec); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("ResolvePeer(%q) error = %v, want %q", spec, err, wantErr)
		}
	}
}

func TestApprovalAndOverlap(t *testing.T) {
	st := status()
	adv := pfx("10.250.0.10/32", "10.250.0.11/32")
	if got := Approval(st, adv, pfx("10.250.0.10/32")[0]); got != RouteActive {
		t.Errorf("approved route = %v", got)
	}
	if got := Approval(st, adv, pfx("10.250.0.11/32")[0]); got != RoutePending {
		t.Errorf("unapproved route = %v", got)
	}
	if got := Approval(st, adv, pfx("10.250.0.12/32")[0]); got != RouteNotAdvertised {
		t.Errorf("missing route = %v", got)
	}
	if got := PeerRouteOverlaps(st, netip.MustParseAddr("10.99.3.4")); len(got) != 1 || !strings.Contains(got[0], "router2") {
		t.Errorf("overlaps = %v", got)
	}
	if got := PeerRouteOverlaps(st, netip.MustParseAddr("10.250.0.10")); len(got) != 0 {
		t.Errorf("self routes reported as overlap: %v", got)
	}
}
