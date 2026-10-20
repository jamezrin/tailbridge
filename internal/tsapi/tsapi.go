// Package tsapi adapts the running tailscaled's Local API for Tailbridge:
// peer resolution, route approval inspection and ownership-preserving edits
// of AdvertiseRoutes.
package tsapi

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
)

// Client is the subset of the Local API Tailbridge uses.
type Client interface {
	Status(ctx context.Context) (*ipnstate.Status, error)
	GetPrefs(ctx context.Context) (*ipn.Prefs, error)
	EditPrefs(ctx context.Context, mp *ipn.MaskedPrefs) (*ipn.Prefs, error)
}

// New returns a client talking only to the given tailscaled socket.
func New(socket string) Client {
	return &local.Client{Socket: socket, UseSocketOnly: true}
}

// Peer is a resolved tailnet peer.
type Peer struct {
	Name   string // MagicDNS name without trailing dot, or hostname
	IP     netip.Addr
	Online bool
}

// ResolvePeer finds the peer identified by spec: a Tailscale IPv4 address,
// a full MagicDNS name, its first label, or the peer's hostname
// (case-insensitive).
func ResolvePeer(st *ipnstate.Status, spec string) (Peer, error) {
	spec = strings.TrimSuffix(strings.TrimSpace(spec), ".")
	if spec == "" {
		return Peer{}, fmt.Errorf("empty peer")
	}
	ip, ipErr := netip.ParseAddr(spec)
	var matches []*ipnstate.PeerStatus
	for _, p := range st.Peer {
		if ipErr == nil {
			if slices.Contains(p.TailscaleIPs, ip) {
				matches = append(matches, p)
			}
			continue
		}
		if nameMatches(p, spec) {
			matches = append(matches, p)
		}
	}
	if len(matches) == 0 && st.Self != nil {
		if (ipErr == nil && slices.Contains(st.Self.TailscaleIPs, ip)) || (ipErr != nil && nameMatches(st.Self, spec)) {
			return Peer{}, fmt.Errorf("peer %q is this gateway itself", spec)
		}
	}
	switch len(matches) {
	case 0:
		return Peer{}, fmt.Errorf("no tailnet peer matches %q", spec)
	case 1:
	default:
		var names []string
		for _, m := range matches {
			names = append(names, displayName(m))
		}
		slices.Sort(names)
		return Peer{}, fmt.Errorf("peer %q is ambiguous: matches %s; use the full MagicDNS name or IP", spec, strings.Join(names, ", "))
	}
	p := matches[0]
	v4 := firstV4(p.TailscaleIPs)
	if !v4.IsValid() {
		return Peer{}, fmt.Errorf("peer %q has no Tailscale IPv4 address", spec)
	}
	return Peer{Name: displayName(p), IP: v4, Online: p.Online}, nil
}

func nameMatches(p *ipnstate.PeerStatus, spec string) bool {
	dns := strings.TrimSuffix(p.DNSName, ".")
	first, _, _ := strings.Cut(dns, ".")
	return strings.EqualFold(dns, spec) || (first != "" && strings.EqualFold(first, spec)) || strings.EqualFold(p.HostName, spec)
}

func displayName(p *ipnstate.PeerStatus) string {
	if d := strings.TrimSuffix(p.DNSName, "."); d != "" {
		return d
	}
	return p.HostName
}

func firstV4(ips []netip.Addr) netip.Addr {
	for _, ip := range ips {
		if ip.Is4() {
			return ip
		}
	}
	return netip.Addr{}
}

// RouteApproval describes control-plane acceptance of an advertised route.
type RouteApproval int

const (
	RouteNotAdvertised RouteApproval = iota
	// RoutePending: advertised but not approved (or not primary) yet.
	RoutePending
	// RouteActive: this node is the primary router for the route.
	RouteActive
)

func (r RouteApproval) String() string {
	switch r {
	case RoutePending:
		return "pending-approval"
	case RouteActive:
		return "approved"
	}
	return "not-advertised"
}

// Approval reports whether route is active for this node according to status.
func Approval(st *ipnstate.Status, advertised []netip.Prefix, route netip.Prefix) RouteApproval {
	if !slices.Contains(advertised, route) {
		return RouteNotAdvertised
	}
	if st != nil && st.Self != nil && st.Self.PrimaryRoutes != nil && slices.Contains(st.Self.PrimaryRoutes.AsSlice(), route) {
		return RouteActive
	}
	return RoutePending
}

// PeerRouteOverlaps returns the names of other peers whose primary routes
// contain addr, which would conflict with advertising it from this gateway.
func PeerRouteOverlaps(st *ipnstate.Status, addr netip.Addr) []string {
	var out []string
	for _, p := range st.Peer {
		if p.PrimaryRoutes == nil {
			continue
		}
		for _, r := range p.PrimaryRoutes.All() {
			if r.Bits() > 0 && r.Contains(addr) {
				out = append(out, fmt.Sprintf("%s (%s)", displayName(p), r))
			}
		}
	}
	slices.Sort(out)
	return out
}

// MergeRoutes computes the next AdvertiseRoutes value. It removes routes
// Tailbridge owns but no longer wants, adds wanted routes, and never touches
// routes it does not own. Wanted routes that were already advertised by
// someone else are left unowned so that they survive Tailbridge cleanup.
func MergeRoutes(current, owned, wanted []netip.Prefix) (next, nextOwned []netip.Prefix) {
	for _, r := range current {
		if slices.Contains(owned, r) && !slices.Contains(wanted, r) {
			continue
		}
		next = append(next, r)
	}
	for _, r := range wanted {
		switch {
		case !slices.Contains(current, r):
			next = append(next, r)
			nextOwned = append(nextOwned, r)
		case slices.Contains(owned, r):
			nextOwned = append(nextOwned, r)
		}
	}
	return next, nextOwned
}

// SameRoutes reports whether a and b contain the same set of prefixes.
func SameRoutes(a, b []netip.Prefix) bool {
	if len(a) != len(b) {
		return false
	}
	for _, r := range a {
		if !slices.Contains(b, r) {
			return false
		}
	}
	return true
}

// SetAdvertiseRoutes writes routes and verifies the daemon reports exactly
// that set afterwards. EditPrefs replaces the whole list, so callers must
// compute routes from a fresh GetPrefs immediately before calling this.
func SetAdvertiseRoutes(ctx context.Context, c Client, routes []netip.Prefix) error {
	if routes == nil {
		routes = []netip.Prefix{}
	}
	got, err := c.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs:              ipn.Prefs{AdvertiseRoutes: routes},
		AdvertiseRoutesSet: true,
	})
	if err != nil {
		return fmt.Errorf("update tailscale advertised routes: %w", err)
	}
	if !SameRoutes(got.AdvertiseRoutes, routes) {
		return fmt.Errorf("tailscaled reported advertised routes %v after setting %v (concurrent change?)", got.AdvertiseRoutes, routes)
	}
	return nil
}
