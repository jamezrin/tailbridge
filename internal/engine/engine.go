// Package engine reconciles Tailbridge's desired configuration with the
// host: LAN aliases, the Tailbridge nftables table and tailscaled's
// advertised routes.
package engine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"

	"github.com/jamezrin/tailbridge/internal/config"
	"github.com/jamezrin/tailbridge/internal/netif"
	"github.com/jamezrin/tailbridge/internal/nft"
	"github.com/jamezrin/tailbridge/internal/state"
	"github.com/jamezrin/tailbridge/internal/sysctl"
	"github.com/jamezrin/tailbridge/internal/tsapi"
)

// Engine holds the adapters used to observe and change the host.
type Engine struct {
	TS    tsapi.Client
	Net   netif.Net
	NFT   nft.Backend
	Fwd   sysctl.Forwarding
	State state.Dir
}

// Mapping states.
const (
	StateActive          = "active"
	StatePendingApproval = "pending-approval"
	StateError           = "error"
)

// MappingStatus is the observed state of one mapping.
type MappingStatus struct {
	Name       string             `json:"name"`
	Kind       config.Kind        `json:"kind"`
	Interface  string             `json:"interface"`
	IP         netip.Addr         `json:"ip"`
	Peer       string             `json:"peer,omitempty"`
	PeerName   string             `json:"peerName,omitempty"`
	PeerIP     netip.Addr         `json:"peerIP,omitzero"`
	PeerOnline *bool              `json:"peerOnline,omitempty"`
	Target     netip.Addr         `json:"target,omitzero"`
	Sources    []netip.Prefix     `json:"allowSources,omitempty"`
	Ports      []config.PortRange `json:"ports,omitempty"`
	SNAT       *bool              `json:"snat,omitempty"`
	Route      string             `json:"route,omitempty"`
	State      string             `json:"state"`
	Errors     []string           `json:"errors,omitempty"`
	Warnings   []string           `json:"warnings,omitempty"`
}

func (s *MappingStatus) fail(format string, args ...any) {
	s.State = StateError
	s.Errors = append(s.Errors, fmt.Sprintf(format, args...))
}

func (s *MappingStatus) warn(format string, args ...any) {
	s.Warnings = append(s.Warnings, fmt.Sprintf(format, args...))
}

// Plan lists the changes a reconcile would make (or made).
type Plan struct {
	EnableForwarding bool              `json:"enableForwarding,omitempty"`
	WithdrawRoutes   []netip.Prefix    `json:"withdrawRoutes,omitempty"`
	AdvertiseRoutes  []netip.Prefix    `json:"advertiseRoutes,omitempty"`
	DelAddrs         []state.OwnedAddr `json:"removeAddresses,omitempty"`
	AddAddrs         []state.OwnedAddr `json:"addAddresses,omitempty"`
	ReplaceNFT       bool              `json:"replaceFirewall,omitempty"`
	DeleteNFT        bool              `json:"deleteFirewall,omitempty"`
	// Ruleset is the full desired Tailbridge table (empty when no mappings).
	Ruleset string `json:"ruleset,omitempty"`
}

// Empty reports whether nothing needs to change.
func (p *Plan) Empty() bool {
	return !p.EnableForwarding && len(p.WithdrawRoutes) == 0 && len(p.AdvertiseRoutes) == 0 &&
		len(p.DelAddrs) == 0 && len(p.AddAddrs) == 0 && !p.ReplaceNFT && !p.DeleteNFT
}

// Steps describes the plan as ordered, human-readable steps.
func (p *Plan) Steps() []string {
	var out []string
	if p.EnableForwarding {
		out = append(out, "enable net.ipv4.ip_forward")
	}
	for _, r := range p.WithdrawRoutes {
		out = append(out, "withdraw advertised route "+r.String())
	}
	for _, a := range p.DelAddrs {
		out = append(out, fmt.Sprintf("remove address %s/32 from %s", a.Addr, a.Interface))
	}
	if p.ReplaceNFT {
		out = append(out, fmt.Sprintf("replace nftables table %s %s", nft.Family, nft.Table))
	}
	if p.DeleteNFT {
		out = append(out, fmt.Sprintf("delete nftables table %s %s", nft.Family, nft.Table))
	}
	for _, a := range p.AddAddrs {
		out = append(out, fmt.Sprintf("add address %s/32 to %s", a.Addr, a.Interface))
	}
	for _, r := range p.AdvertiseRoutes {
		out = append(out, "advertise route "+r.String())
	}
	return out
}

// Result is the outcome of a reconcile.
type Result struct {
	Mappings []*MappingStatus `json:"mappings"`
	Plan     Plan             `json:"plan"`
	Warnings []string         `json:"warnings,omitempty"`
	Applied  bool             `json:"applied"`
}

// Mapping returns the status of the named mapping.
func (r *Result) Mapping(name string) *MappingStatus {
	for _, m := range r.Mappings {
		if m.Name == name {
			return m
		}
	}
	return nil
}

// observation is what reconcile learned about tailscaled.
type observation struct {
	status *ipnstate.Status
	prefs  *ipn.Prefs
}

func (e *Engine) observeTailscale(ctx context.Context, res *Result) observation {
	var obs observation
	st, err := e.TS.Status(ctx)
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("tailscaled unavailable (%v): using cached peer addresses and leaving advertised routes unchanged", err))
		return obs
	}
	if st.BackendState != ipn.Running.String() {
		res.Warnings = append(res.Warnings, fmt.Sprintf("tailscaled backend state is %q, not Running", st.BackendState))
	}
	prefs, err := e.TS.GetPrefs(ctx)
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("read tailscale prefs: %v: leaving advertised routes unchanged", err))
	}
	obs.status, obs.prefs = st, prefs
	return obs
}

// Options control a reconcile.
type Options struct {
	DryRun bool
}

// Reconcile converges the host toward cfg. With DryRun it only computes the
// plan and statuses.
func (e *Engine) Reconcile(ctx context.Context, cfg *config.Config, opts Options) (*Result, error) {
	st, err := e.State.Load()
	if err != nil {
		return nil, err
	}
	res := &Result{}
	obs := e.observeTailscale(ctx, res)
	tsIf := cfg.TSInterface()
	if !e.Net.LinkExists(tsIf) {
		res.Warnings = append(res.Warnings, fmt.Sprintf("tailscale interface %s does not exist", tsIf))
	}

	rules := nft.Rules{TSInterface: tsIf}
	var wantAddrs []state.OwnedAddr
	var wantRoutes []netip.Prefix
	for i := range cfg.Mappings {
		m := &cfg.Mappings[i]
		ms := &MappingStatus{Name: m.Name, Kind: m.Kind, Interface: m.Interface, IP: m.IP, Peer: m.Peer, Target: m.Target, Ports: m.Ports, State: StateActive}
		res.Mappings = append(res.Mappings, ms)
		switch m.Kind {
		case config.KindLAN:
			if r, ok := e.evalLAN(m, ms, st, obs, tsIf); ok {
				rules.LAN = append(rules.LAN, r)
				wantAddrs = append(wantAddrs, state.OwnedAddr{Interface: m.Interface, Addr: m.IP})
			}
		case config.KindTailnet:
			if r, ok := e.evalTailnet(m, ms, st, obs); ok {
				rules.Tailnet = append(rules.Tailnet, r)
				wantRoutes = append(wantRoutes, netip.PrefixFrom(m.IP, 32))
			}
		}
	}

	if err := e.plan(res, st, obs, &rules, wantAddrs, wantRoutes); err != nil {
		return res, err
	}
	if opts.DryRun {
		e.fillRouteStatus(res, obs)
		return res, nil
	}
	if res.Plan.Empty() {
		// Persist refreshed peer addresses.
		err = e.State.Save(st)
		e.fillRouteStatus(res, obs)
		return res, err
	}
	err = e.apply(ctx, res, st, wantRoutes)
	res.Applied = true
	// Refresh approval information after changing routes.
	if obs.prefs != nil && (len(res.Plan.AdvertiseRoutes) > 0 || len(res.Plan.WithdrawRoutes) > 0) {
		if s, serr := e.TS.Status(ctx); serr == nil {
			obs.status = s
		}
		if p, perr := e.TS.GetPrefs(ctx); perr == nil {
			obs.prefs = p
		}
	}
	e.fillRouteStatus(res, obs)
	return res, err
}

func (e *Engine) evalLAN(m *config.Mapping, ms *MappingStatus, st *state.State, obs observation, tsIf string) (nft.LANRule, bool) {
	if !e.Net.LinkExists(m.Interface) {
		ms.fail("interface %s does not exist", m.Interface)
		return nft.LANRule{}, false
	}
	subnets, err := e.Net.Subnets(m.Interface)
	if err != nil {
		ms.fail("%v", err)
		return nft.LANRule{}, false
	}

	// Resolve the peer, falling back to the last known address.
	var peerIP netip.Addr
	if obs.status != nil {
		p, err := tsapi.ResolvePeer(obs.status, m.Peer)
		if err == nil {
			peerIP = p.IP
			ms.PeerName = p.Name
			online := p.Online
			ms.PeerOnline = &online
			if !online {
				ms.warn("peer %s is offline", p.Name)
			}
			st.Peers[m.Name] = p.IP
		} else if cached, ok := st.Peers[m.Name]; ok {
			peerIP = cached
			ms.warn("%v; using last known address %s", err, cached)
		} else {
			ms.fail("%v", err)
		}
	} else if cached, ok := st.Peers[m.Name]; ok {
		peerIP = cached
		ms.warn("tailscaled unavailable; using last known peer address %s", cached)
	} else {
		ms.fail("tailscaled unavailable and peer %q was never resolved", m.Peer)
	}
	ms.PeerIP = peerIP

	if owner, err := e.Net.AddrOwner(m.IP); err != nil {
		ms.fail("inspect addresses: %v", err)
	} else if owner != "" && !st.HasAddr(owner, m.IP) {
		ms.fail("address %s is already assigned to %s and not owned by tailbridge", m.IP, owner)
	} else if owner == "" && !st.HasAddr(m.Interface, m.IP) {
		if mac, err := e.Net.Neighbor(m.Interface, m.IP); err == nil && mac != "" {
			ms.fail("another LAN host (%s) is using %s", mac, m.IP)
		}
	}
	if !containsAddr(subnets, m.IP) {
		ms.warn("alias %s is outside the connected subnets of %s (%s); LAN clients will need a route to reach it", m.IP, m.Interface, prefixList(subnets))
	}

	srcs := m.AllowSources
	if len(srcs) == 0 {
		srcs = subnets
		if len(srcs) == 0 {
			ms.fail("interface %s has no IPv4 subnet to default allowed sources to; set allowed sources explicitly", m.Interface)
		}
	}
	ms.Sources = srcs

	if peerIP.IsValid() {
		if via, err := e.Net.RouteVia(peerIP); err != nil {
			ms.warn("%v", err)
		} else if via != tsIf {
			ms.warn("peer %s is routed via %s, not %s", peerIP, via, tsIf)
		}
	}
	if ms.State == StateError {
		return nft.LANRule{}, false
	}
	return nft.LANRule{Name: m.Name, Interface: m.Interface, Alias: m.IP, Peer: peerIP, Sources: srcs, Ports: m.Ports}, true
}

func (e *Engine) evalTailnet(m *config.Mapping, ms *MappingStatus, st *state.State, obs observation) (nft.TailnetRule, bool) {
	snat := !m.NoSNAT
	ms.SNAT = &snat
	ms.Sources = m.AllowSources
	if !e.Net.LinkExists(m.Interface) {
		ms.fail("interface %s does not exist", m.Interface)
		return nft.TailnetRule{}, false
	}
	if owner, err := e.Net.AddrOwner(m.IP); err != nil {
		ms.fail("inspect addresses: %v", err)
	} else if owner != "" && !st.HasAddr(owner, m.IP) {
		ms.fail("virtual ip %s is assigned to local interface %s", m.IP, owner)
	}
	if routes, err := e.Net.CoveringRoutes(m.IP); err != nil {
		ms.fail("inspect routes: %v", err)
	} else if len(routes) > 0 {
		ms.fail("virtual ip %s overlaps existing routes: %s", m.IP, strings.Join(routes, "; "))
	}
	if obs.status != nil {
		if o := tsapi.PeerRouteOverlaps(obs.status, m.IP); len(o) > 0 {
			ms.fail("virtual ip %s overlaps routes advertised by other tailnet nodes: %s", m.IP, strings.Join(o, ", "))
		}
	}
	if via, err := e.Net.RouteVia(m.Target); err != nil {
		ms.fail("%v", err)
	} else if via != m.Interface {
		ms.fail("target %s is routed via %s, not %s", m.Target, via, m.Interface)
	}
	if ms.State == StateError {
		return nft.TailnetRule{}, false
	}
	return nft.TailnetRule{Name: m.Name, Interface: m.Interface, VIP: m.IP, Target: m.Target, Sources: m.AllowSources, Ports: m.Ports, SNAT: snat}, true
}

func (e *Engine) fillRouteStatus(res *Result, obs observation) {
	for _, ms := range res.Mappings {
		if ms.Kind != config.KindTailnet || ms.State == StateError {
			continue
		}
		if obs.status == nil || obs.prefs == nil {
			ms.Route = "unknown"
			continue
		}
		a := tsapi.Approval(obs.status, obs.prefs.AdvertiseRoutes, netip.PrefixFrom(ms.IP, 32))
		ms.Route = a.String()
		switch a {
		case tsapi.RoutePending:
			ms.State = StatePendingApproval
			ms.warn("route %s/32 is advertised but not approved for this gateway; approve it in the admin console or with autoApprovers", ms.IP)
		case tsapi.RouteNotAdvertised:
			if res.Applied || res.Plan.Empty() {
				ms.fail("route %s/32 is not advertised", ms.IP)
			}
		}
	}
}

func (e *Engine) plan(res *Result, st *state.State, obs observation, rules *nft.Rules, wantAddrs []state.OwnedAddr, wantRoutes []netip.Prefix) error {
	p := &res.Plan
	text, hash := nft.Render(rules)
	curHash, present, err := e.NFT.CurrentHash()
	if err != nil {
		return fmt.Errorf("inspect nftables: %w", err)
	}
	if rules.Empty() {
		p.DeleteNFT = present
	} else {
		p.Ruleset = text
		p.ReplaceNFT = !present || curHash != hash
	}

	for _, a := range st.Addresses {
		if !slices.Contains(wantAddrs, a) {
			p.DelAddrs = append(p.DelAddrs, a)
		}
	}
	for _, a := range wantAddrs {
		owner, err := e.Net.AddrOwner(a.Addr)
		if err != nil {
			return err
		}
		if owner != a.Interface {
			p.AddAddrs = append(p.AddAddrs, a)
		}
	}

	if obs.prefs != nil {
		cur := obs.prefs.AdvertiseRoutes
		next, _ := tsapi.MergeRoutes(cur, st.Routes, wantRoutes)
		for _, r := range cur {
			if !slices.Contains(next, r) {
				p.WithdrawRoutes = append(p.WithdrawRoutes, r)
			}
		}
		for _, r := range next {
			if !slices.Contains(cur, r) {
				p.AdvertiseRoutes = append(p.AdvertiseRoutes, r)
			}
		}
	}

	if !rules.Empty() {
		on, err := e.Fwd.Enabled()
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("read ip_forward: %v", err))
		} else if !on {
			p.EnableForwarding = true
		}
	}
	return nil
}

// apply executes res.Plan in a safe order:
//
//  1. withdraw routes no longer wanted (tailscaled stops accepting traffic
//     for them before their NAT rules disappear);
//  2. remove stale aliases (before their drop rules disappear);
//  3. atomically replace the firewall table;
//  4. add new aliases (only once their rules exist);
//  5. advertise new routes (only once their rules exist).
//
// Ownership is recorded in the state file before each resource is created,
// so a crash never leaves a Tailbridge-created resource that looks foreign.
func (e *Engine) apply(ctx context.Context, res *Result, st *state.State, wantRoutes []netip.Prefix) error {
	p := &res.Plan
	var errs []error
	save := func() {
		if err := e.State.Save(st); err != nil {
			errs = append(errs, fmt.Errorf("save state: %w", err))
		}
	}

	if p.EnableForwarding {
		if err := e.Fwd.Enable(); err != nil {
			errs = append(errs, fmt.Errorf("enable ip_forward: %w", err))
		}
	}

	if len(p.WithdrawRoutes) > 0 {
		if err := e.withdrawRoutes(ctx, st, p.WithdrawRoutes); err != nil {
			errs = append(errs, err)
		}
		save()
	}

	for _, a := range p.DelAddrs {
		if err := e.Net.DelAddr(a.Interface, a.Addr); err != nil {
			errs = append(errs, err)
			continue
		}
		st.DelAddr(a.Interface, a.Addr)
	}
	save()

	switch {
	case p.ReplaceNFT:
		if err := e.NFT.Replace(p.Ruleset); err != nil {
			// Without rules, adding aliases or routes would expose the
			// gateway itself, so stop here.
			save()
			return errors.Join(append(errs, fmt.Errorf("apply firewall rules: %w", err))...)
		}
	case p.DeleteNFT:
		if err := e.NFT.Delete(); err != nil {
			errs = append(errs, fmt.Errorf("delete firewall table: %w", err))
		}
	}

	for _, a := range p.AddAddrs {
		st.AddAddr(a.Interface, a.Addr)
		save()
		if err := e.Net.AddAddr(a.Interface, a.Addr); err != nil {
			st.DelAddr(a.Interface, a.Addr)
			errs = append(errs, err)
			if ms := res.mappingByIP(a.Addr); ms != nil {
				ms.fail("%v", err)
			}
		}
	}
	save()

	if len(p.AdvertiseRoutes) > 0 {
		if err := e.advertiseRoutes(ctx, st, wantRoutes); err != nil {
			errs = append(errs, err)
		}
		save()
	}
	return errors.Join(errs...)
}

func (r *Result) mappingByIP(ip netip.Addr) *MappingStatus {
	for _, m := range r.Mappings {
		if m.IP == ip {
			return m
		}
	}
	return nil
}

// withdrawRoutes removes owned routes from a fresh copy of the prefs.
func (e *Engine) withdrawRoutes(ctx context.Context, st *state.State, withdraw []netip.Prefix) error {
	prefs, err := e.TS.GetPrefs(ctx)
	if err != nil {
		return fmt.Errorf("read tailscale prefs: %w", err)
	}
	var next []netip.Prefix
	for _, r := range prefs.AdvertiseRoutes {
		if slices.Contains(withdraw, r) && slices.Contains(st.Routes, r) {
			continue
		}
		next = append(next, r)
	}
	if !tsapi.SameRoutes(next, prefs.AdvertiseRoutes) {
		if err := tsapi.SetAdvertiseRoutes(ctx, e.TS, next); err != nil {
			return err
		}
	}
	st.Routes = slices.DeleteFunc(st.Routes, func(r netip.Prefix) bool { return slices.Contains(withdraw, r) })
	return nil
}

// advertiseRoutes merges wanted routes into a fresh copy of the prefs.
func (e *Engine) advertiseRoutes(ctx context.Context, st *state.State, want []netip.Prefix) error {
	prefs, err := e.TS.GetPrefs(ctx)
	if err != nil {
		return fmt.Errorf("read tailscale prefs: %w", err)
	}
	next, nextOwned := tsapi.MergeRoutes(prefs.AdvertiseRoutes, st.Routes, want)
	// Record intended ownership before writing.
	for _, r := range nextOwned {
		if !slices.Contains(st.Routes, r) {
			st.Routes = append(st.Routes, r)
		}
	}
	if err := e.State.Save(st); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	if !tsapi.SameRoutes(next, prefs.AdvertiseRoutes) {
		if err := tsapi.SetAdvertiseRoutes(ctx, e.TS, next); err != nil {
			return err
		}
	}
	st.Routes = nextOwned
	return nil
}

// Down removes every Tailbridge-owned resource: owned advertised routes,
// owned aliases and the Tailbridge firewall table. Configuration is kept.
// net.ipv4.ip_forward is deliberately left unchanged.
func (e *Engine) Down(ctx context.Context, dryRun bool) (*Plan, error) {
	st, err := e.State.Load()
	if err != nil {
		return nil, err
	}
	p := &Plan{DelAddrs: slices.Clone(st.Addresses)}
	var errs []error
	var prefs *ipn.Prefs
	if len(st.Routes) > 0 {
		prefs, err = e.TS.GetPrefs(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("read tailscale prefs (needed to withdraw %s): %w", prefixList(st.Routes), err))
		}
	}
	if prefs != nil {
		for _, r := range prefs.AdvertiseRoutes {
			if slices.Contains(st.Routes, r) {
				p.WithdrawRoutes = append(p.WithdrawRoutes, r)
			}
		}
	}
	_, present, err := e.NFT.CurrentHash()
	if err != nil {
		return p, fmt.Errorf("inspect nftables: %w", err)
	}
	p.DeleteNFT = present
	if dryRun {
		return p, errors.Join(errs...)
	}

	if prefs != nil {
		if err := e.withdrawRoutes(ctx, st, st.Routes); err != nil {
			errs = append(errs, err)
		}
	}
	for _, a := range slices.Clone(st.Addresses) {
		if err := e.Net.DelAddr(a.Interface, a.Addr); err != nil {
			errs = append(errs, err)
			continue
		}
		st.DelAddr(a.Interface, a.Addr)
	}
	if p.DeleteNFT {
		if err := e.NFT.Delete(); err != nil {
			errs = append(errs, fmt.Errorf("delete firewall table: %w", err))
		}
	}
	if err := e.State.Save(st); err != nil {
		errs = append(errs, err)
	}
	return p, errors.Join(errs...)
}

func containsAddr(ps []netip.Prefix, a netip.Addr) bool {
	return slices.ContainsFunc(ps, func(p netip.Prefix) bool { return p.Contains(a) })
}

func prefixList(ps []netip.Prefix) string {
	if len(ps) == 0 {
		return "none"
	}
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = p.String()
	}
	return strings.Join(s, ", ")
}
