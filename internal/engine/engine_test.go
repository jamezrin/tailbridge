package engine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"

	"github.com/jamezrin/tailbridge/internal/config"
	"github.com/jamezrin/tailbridge/internal/nft"
	"github.com/jamezrin/tailbridge/internal/state"
)

// fakeNet models interfaces with addresses; routes are derived from subnets.
type fakeNet struct {
	addrs map[string][]netip.Prefix // iface -> addresses
	log   *[]string
}

func (f *fakeNet) LinkExists(name string) bool { _, ok := f.addrs[name]; return ok }
func (f *fakeNet) Subnets(name string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, p := range f.addrs[name] {
		if p.Bits() < 32 {
			out = append(out, p.Masked())
		}
	}
	return out, nil
}
func (f *fakeNet) AddrOwner(a netip.Addr) (string, error) {
	for ifc, ps := range f.addrs {
		for _, p := range ps {
			if p.Addr() == a {
				return ifc, nil
			}
		}
	}
	return "", nil
}
func (f *fakeNet) AddAddr(ifc string, a netip.Addr) error {
	*f.log = append(*f.log, "addaddr "+a.String())
	f.addrs[ifc] = append(f.addrs[ifc], netip.PrefixFrom(a, 32))
	return nil
}
func (f *fakeNet) DelAddr(ifc string, a netip.Addr) error {
	*f.log = append(*f.log, "deladdr "+a.String())
	f.addrs[ifc] = slices.DeleteFunc(f.addrs[ifc], func(p netip.Prefix) bool { return p.Addr() == a })
	return nil
}
func (f *fakeNet) RouteVia(dst netip.Addr) (string, error) {
	if netip.MustParsePrefix("100.64.0.0/10").Contains(dst) {
		return "tailscale0", nil
	}
	for ifc := range f.addrs {
		subs, _ := f.Subnets(ifc)
		for _, s := range subs {
			if s.Contains(dst) {
				return ifc, nil
			}
		}
	}
	return "", fmt.Errorf("no route to %s", dst)
}
func (f *fakeNet) CoveringRoutes(a netip.Addr) ([]string, error) {
	var out []string
	for ifc := range f.addrs {
		subs, _ := f.Subnets(ifc)
		for _, s := range subs {
			if s.Contains(a) {
				out = append(out, s.String()+" dev "+ifc)
			}
		}
	}
	return out, nil
}
func (f *fakeNet) Neighbor(string, netip.Addr) (string, error) { return "", nil }

type fakeNFT struct {
	hash      string
	present   bool
	failApply bool
	text      string
	log       *[]string
}

func (f *fakeNFT) CurrentHash() (string, bool, error) { return f.hash, f.present, nil }
func (f *fakeNFT) Replace(text string) error {
	if f.failApply {
		return errors.New("boom")
	}
	*f.log = append(*f.log, "nft replace")
	f.text = text
	_, after, _ := strings.Cut(text, "sha256:")
	f.hash, f.present = after[:16], true
	return nil
}
func (f *fakeNFT) Delete() error {
	*f.log = append(*f.log, "nft delete")
	f.hash, f.present = "", false
	return nil
}
func (f *fakeNFT) Chains() ([]nft.Chain, error) { return nil, nil }

type fakeTS struct {
	st       *ipnstate.Status
	routes   []netip.Prefix
	down     bool
	failEdit bool
	writes   int
	log      *[]string
}

func (f *fakeTS) Status(context.Context) (*ipnstate.Status, error) {
	if f.down {
		return nil, errors.New("connection refused")
	}
	return f.st, nil
}
func (f *fakeTS) GetPrefs(context.Context) (*ipn.Prefs, error) {
	if f.down {
		return nil, errors.New("connection refused")
	}
	return &ipn.Prefs{AdvertiseRoutes: slices.Clone(f.routes)}, nil
}
func (f *fakeTS) EditPrefs(_ context.Context, mp *ipn.MaskedPrefs) (*ipn.Prefs, error) {
	if f.failEdit {
		return nil, errors.New("edit refused")
	}
	f.writes++
	*f.log = append(*f.log, fmt.Sprint("routes ", mp.AdvertiseRoutes))
	f.routes = slices.Clone(mp.AdvertiseRoutes)
	return &ipn.Prefs{AdvertiseRoutes: slices.Clone(f.routes)}, nil
}

type fakeFwd struct{ on bool }

func (f *fakeFwd) Enabled() (bool, error) { return f.on, nil }
func (f *fakeFwd) Enable() error          { f.on = true; return nil }

type harness struct {
	eng *Engine
	net *fakeNet
	nft *fakeNFT
	ts  *fakeTS
	fwd *fakeFwd
	log []string
}

func newHarness(t *testing.T) *harness {
	h := &harness{}
	peer := &ipnstate.PeerStatus{ID: "n1", HostName: "device-b", DNSName: "device-b.ts.net.", Online: true,
		TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.101.102.103")}}
	h.net = &fakeNet{log: &h.log, addrs: map[string][]netip.Prefix{
		"eth0":       {netip.MustParsePrefix("192.168.1.1/24")},
		"tailscale0": {netip.MustParsePrefix("100.64.0.1/32")},
	}}
	h.nft = &fakeNFT{log: &h.log}
	h.ts = &fakeTS{log: &h.log, routes: pfx("0.0.0.0/0", "::/0"), st: &ipnstate.Status{
		BackendState: "Running",
		Self:         &ipnstate.PeerStatus{HostName: "gw", DNSName: "gw.ts.net.", TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.1")}},
		Peer:         map[key.NodePublic]*ipnstate.PeerStatus{key.NewNode().Public(): peer},
	}}
	h.fwd = &fakeFwd{}
	h.eng = &Engine{TS: h.ts, Net: h.net, NFT: h.nft, Fwd: h.fwd, State: state.Dir(t.TempDir())}
	return h
}

func pfx(ss ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

func cfgWith(ms ...config.Mapping) *config.Config { return &config.Config{Mappings: ms} }

func lanMapping() config.Mapping {
	return config.Mapping{Name: "b", Kind: config.KindLAN, Interface: "eth0", IP: netip.MustParseAddr("192.168.1.100"), Peer: "device-b"}
}

func tailnetMapping() config.Mapping {
	return config.Mapping{Name: "p", Kind: config.KindTailnet, Interface: "eth0", IP: netip.MustParseAddr("10.250.0.10"), Target: netip.MustParseAddr("192.168.1.80")}
}

func TestReconcileAppliesInSafeOrderAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cfg := cfgWith(lanMapping(), tailnetMapping())
	res, err := h.eng.Reconcile(ctx, cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"nft replace", "addaddr 192.168.1.100", "routes [0.0.0.0/0 ::/0 10.250.0.10/32]"}
	if !slices.Equal(h.log, want) {
		t.Fatalf("operations = %q, want %q", h.log, want)
	}
	if !h.fwd.on {
		t.Error("forwarding not enabled")
	}
	if got := res.Mapping("b"); got.State != StateActive || got.PeerIP != netip.MustParseAddr("100.101.102.103") {
		t.Errorf("lan status = %+v", got)
	}
	if got := res.Mapping("p"); got.State != StatePendingApproval {
		t.Errorf("tailnet status = %+v (no PrimaryRoutes, so pending)", got)
	}
	st, _ := h.eng.State.Load()
	if !st.HasAddr("eth0", netip.MustParseAddr("192.168.1.100")) || !slices.Equal(st.Routes, pfx("10.250.0.10/32")) {
		t.Errorf("ownership not recorded: %+v", st)
	}

	h.log = nil
	res, err = h.eng.Reconcile(ctx, cfg, Options{})
	if err != nil || res.Applied || len(h.log) != 0 {
		t.Fatalf("second reconcile changed things: applied=%v ops=%q err=%v", res.Applied, h.log, err)
	}
}

func TestRemovalWithdrawsBeforeFirewallAndKeepsForeignState(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.eng.Reconcile(ctx, cfgWith(lanMapping(), tailnetMapping()), Options{}); err != nil {
		t.Fatal(err)
	}
	h.ts.routes = append(h.ts.routes, pfx("192.0.2.0/24")...) // added by the admin later
	h.log = nil
	if _, err := h.eng.Reconcile(ctx, cfgWith(), Options{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"routes [0.0.0.0/0 ::/0 192.0.2.0/24]", "deladdr 192.168.1.100", "nft delete"}
	if !slices.Equal(h.log, want) {
		t.Fatalf("operations = %q, want %q", h.log, want)
	}
	if !h.fwd.on {
		t.Error("forwarding must never be disabled")
	}
}

func TestFirewallFailureStopsBeforeExposingAnything(t *testing.T) {
	h := newHarness(t)
	h.nft.failApply = true
	_, err := h.eng.Reconcile(context.Background(), cfgWith(lanMapping(), tailnetMapping()), Options{})
	if err == nil {
		t.Fatal("expected error")
	}
	if len(h.log) != 0 || h.ts.writes != 0 {
		t.Fatalf("aliases or routes changed after firewall failure: %q", h.log)
	}
}

func TestCollisionsAreErrors(t *testing.T) {
	h := newHarness(t)
	h.net.addrs["eth1"] = []netip.Prefix{netip.MustParsePrefix("192.168.1.100/32")} // foreign
	vipInLAN := tailnetMapping()
	vipInLAN.IP = netip.MustParseAddr("192.168.1.200")
	badTarget := tailnetMapping()
	badTarget.Name, badTarget.IP, badTarget.Target = "q", netip.MustParseAddr("10.250.0.11"), netip.MustParseAddr("172.16.0.5")
	res, err := h.eng.Reconcile(context.Background(), cfgWith(lanMapping(), vipInLAN, badTarget), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"b": "not owned by tailbridge", "p": "overlaps existing routes", "q": "no route"} {
		ms := res.Mapping(name)
		if ms.State != StateError || !strings.Contains(strings.Join(ms.Errors, ";"), want) {
			t.Errorf("%s: state %s errors %q, want %q", name, ms.State, ms.Errors, want)
		}
	}
	if slices.Contains(h.log, "addaddr 192.168.1.100") || h.ts.writes != 0 {
		t.Errorf("collided mappings were applied: %q", h.log)
	}
}

func TestTailscaledDownUsesCachedPeer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cfg := cfgWith(lanMapping(), tailnetMapping())
	if _, err := h.eng.Reconcile(ctx, cfg, Options{}); err != nil {
		t.Fatal(err)
	}
	h.ts.down = true
	h.nft.present = false // firewall reload while tailscaled is down
	h.log = nil
	res, err := h.eng.Reconcile(ctx, cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ms := res.Mapping("b"); ms.State != StateActive || ms.PeerIP != netip.MustParseAddr("100.101.102.103") {
		t.Errorf("lan mapping = %+v", ms)
	}
	if !slices.Equal(h.log, []string{"nft replace"}) {
		t.Errorf("operations = %q", h.log)
	}
	if len(res.Warnings) == 0 {
		t.Error("expected a tailscaled warning")
	}
}

func TestDownRemovesOnlyOwned(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.ts.routes = append(h.ts.routes, pfx("10.250.0.10/32")...) // already advertised by the admin
	m2 := tailnetMapping()
	m2.Name, m2.IP = "q", netip.MustParseAddr("10.250.0.11")
	if _, err := h.eng.Reconcile(ctx, cfgWith(lanMapping(), tailnetMapping(), m2), Options{}); err != nil {
		t.Fatal(err)
	}
	p, err := h.eng.Down(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.WithdrawRoutes, pfx("10.250.0.11/32")) {
		t.Errorf("withdrew %v", p.WithdrawRoutes)
	}
	if !slices.Equal(h.ts.routes, pfx("0.0.0.0/0", "::/0", "10.250.0.10/32")) {
		t.Errorf("routes after down = %v", h.ts.routes)
	}
	if h.nft.present || len(h.net.addrs["eth0"]) != 1 {
		t.Errorf("leftovers: nft=%v addrs=%v", h.nft.present, h.net.addrs["eth0"])
	}
}

func onlyPeer(h *harness) *ipnstate.PeerStatus {
	for _, p := range h.ts.st.Peer {
		return p
	}
	return nil
}

func TestPeerIdentityIsPinned(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cfg := cfgWith(lanMapping())
	if _, err := h.eng.Reconcile(ctx, cfg, Options{}); err != nil {
		t.Fatal(err)
	}
	// Same node, new address: followed.
	onlyPeer(h).TailscaleIPs = []netip.Addr{netip.MustParseAddr("100.101.102.104")}
	res, err := h.eng.Reconcile(ctx, cfg, Options{})
	if err != nil || res.Mapping("b").PeerIP != netip.MustParseAddr("100.101.102.104") {
		t.Fatalf("address change not followed: %+v %v", res.Mapping("b"), err)
	}
	// A different node now answers to the same name: fail closed.
	onlyPeer(h).ID = "n2"
	h.log = nil
	res, err = h.eng.Reconcile(ctx, cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ms := res.Mapping("b"); ms.State != StateError || !strings.Contains(strings.Join(ms.Errors, ";"), "different node") {
		t.Fatalf("identity change not rejected: %+v", ms)
	}
	if !slices.Contains(h.log, "deladdr 192.168.1.100") {
		t.Errorf("alias kept after identity change: %q", h.log)
	}
}

func TestUnresolvablePeerDoesNotUseCache(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cfg := cfgWith(lanMapping())
	if _, err := h.eng.Reconcile(ctx, cfg, Options{}); err != nil {
		t.Fatal(err)
	}
	onlyPeer(h).DNSName, onlyPeer(h).HostName = "renamed.ts.net.", "renamed"
	res, err := h.eng.Reconcile(ctx, cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ms := res.Mapping("b"); ms.State != StateError {
		t.Fatalf("stale cached address used: %+v", ms)
	}
}

func TestFailedWithdrawalKeepsDropRule(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.eng.Reconcile(ctx, cfgWith(tailnetMapping()), Options{}); err != nil {
		t.Fatal(err)
	}
	h.ts.failEdit = true
	if _, err := h.eng.Reconcile(ctx, cfgWith(), Options{}); err == nil {
		t.Fatal("expected withdrawal error")
	}
	if !h.nft.present || !strings.Contains(h.nft.text, "ip daddr 10.250.0.10 counter drop") {
		t.Fatalf("drop rule for still-advertised route removed:\n%s", h.nft.text)
	}
	if _, err := h.eng.Down(ctx, false); err == nil {
		t.Fatal("down should report the route it could not withdraw")
	}
	if !h.nft.present || !strings.Contains(h.nft.text, "orphan-route") {
		t.Fatalf("down deleted the table while the route is still advertised:\n%s", h.nft.text)
	}
	h.ts.failEdit = false
	if _, err := h.eng.Down(ctx, false); err != nil {
		t.Fatal(err)
	}
	if h.nft.present || slices.Contains(h.ts.routes, pfx("10.250.0.10/32")[0]) {
		t.Fatalf("leftovers after successful down: nft=%v routes=%v", h.nft.present, h.ts.routes)
	}
}

func TestTailscaledDownKeepsDropForStaleRoute(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.eng.Reconcile(ctx, cfgWith(tailnetMapping()), Options{}); err != nil {
		t.Fatal(err)
	}
	h.ts.down = true
	if _, err := h.eng.Reconcile(ctx, cfgWith(), Options{}); err != nil {
		t.Fatal(err)
	}
	if !h.nft.present || !strings.Contains(h.nft.text, "ip daddr 10.250.0.10 counter drop") {
		t.Fatalf("stale route left unprotected:\n%s", h.nft.text)
	}
}
