// Package netif inspects and changes Linux interface addresses and routes
// through netlink.
package netif

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Net is the host networking surface Tailbridge needs.
type Net interface {
	// LinkExists reports whether an interface exists.
	LinkExists(name string) bool
	// Subnets returns the IPv4 connected prefixes on an interface, ignoring
	// /32 host addresses (such as Tailbridge aliases).
	Subnets(name string) ([]netip.Prefix, error)
	// AddrOwner returns the interface holding addr, or "" if none.
	AddrOwner(addr netip.Addr) (string, error)
	AddAddr(iface string, addr netip.Addr) error
	DelAddr(iface string, addr netip.Addr) error
	// RouteVia returns the outgoing interface the kernel would use for dst.
	RouteVia(dst netip.Addr) (string, error)
	// CoveringRoutes lists non-default routes (in any table but local)
	// whose destination contains addr.
	CoveringRoutes(addr netip.Addr) ([]string, error)
	// Neighbor returns the link-layer address of a confirmed (reachable or
	// permanent) neighbour entry for addr on iface, or "". Stale entries
	// are ignored: the host may have left long ago.
	Neighbor(iface string, addr netip.Addr) (string, error)
}

// Netlink is the real implementation.
type Netlink struct{}

var _ Net = Netlink{}

func (Netlink) LinkExists(name string) bool {
	_, err := netlink.LinkByName(name)
	return err == nil
}

func (Netlink) Subnets(name string) ([]netip.Prefix, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("interface %s: %w", name, err)
	}
	addrs, err := netlink.AddrList(l, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, a := range addrs {
		p, ok := toPrefix(a.IPNet)
		if !ok || p.Bits() == 32 {
			continue
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func (Netlink) AddrOwner(addr netip.Addr) (string, error) {
	addrs, err := netlink.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		p, ok := toPrefix(a.IPNet)
		if ok && p.Addr() == addr {
			l, err := netlink.LinkByIndex(a.LinkIndex)
			if err != nil {
				return fmt.Sprintf("ifindex %d", a.LinkIndex), nil
			}
			return l.Attrs().Name, nil
		}
	}
	return "", nil
}

func hostAddr(addr netip.Addr) *netlink.Addr {
	return &netlink.Addr{IPNet: &net.IPNet{IP: addr.AsSlice(), Mask: net.CIDRMask(32, 32)}}
}

func (Netlink) AddAddr(iface string, addr netip.Addr) error {
	l, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("interface %s: %w", iface, err)
	}
	if err := netlink.AddrAdd(l, hostAddr(addr)); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("add %s/32 to %s: %w", addr, iface, err)
	}
	return nil
}

func (Netlink) DelAddr(iface string, addr netip.Addr) error {
	l, err := netlink.LinkByName(iface)
	if err != nil {
		// Interface gone means the address is gone too.
		return nil
	}
	if err := netlink.AddrDel(l, hostAddr(addr)); err != nil && !errors.Is(err, unix.EADDRNOTAVAIL) {
		return fmt.Errorf("remove %s/32 from %s: %w", addr, iface, err)
	}
	return nil
}

func (Netlink) RouteVia(dst netip.Addr) (string, error) {
	routes, err := netlink.RouteGet(dst.AsSlice())
	if err != nil {
		return "", fmt.Errorf("route lookup %s: %w", dst, err)
	}
	if len(routes) == 0 {
		return "", fmt.Errorf("no route to %s", dst)
	}
	if routes[0].Type == unix.RTN_LOCAL {
		return "lo", nil
	}
	l, err := netlink.LinkByIndex(routes[0].LinkIndex)
	if err != nil {
		return "", err
	}
	return l.Attrs().Name, nil
}

func (Netlink) CoveringRoutes(addr netip.Addr) ([]string, error) {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_UNSPEC}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range routes {
		if r.Table == unix.RT_TABLE_LOCAL || r.Dst == nil {
			continue
		}
		p, ok := toPrefix(r.Dst)
		if !ok || p.Bits() == 0 || !p.Contains(addr) {
			continue
		}
		dev := fmt.Sprintf("ifindex %d", r.LinkIndex)
		if l, err := netlink.LinkByIndex(r.LinkIndex); err == nil {
			dev = l.Attrs().Name
		}
		out = append(out, fmt.Sprintf("%s dev %s table %d", p, dev, r.Table))
	}
	return out, nil
}

func (Netlink) Neighbor(iface string, addr netip.Addr) (string, error) {
	l, err := netlink.LinkByName(iface)
	if err != nil {
		return "", err
	}
	neighs, err := netlink.NeighList(l.Attrs().Index, netlink.FAMILY_V4)
	if err != nil {
		return "", err
	}
	for _, n := range neighs {
		ip, ok := netip.AddrFromSlice(n.IP)
		if !ok || ip.Unmap() != addr || len(n.HardwareAddr) == 0 {
			continue
		}
		if n.State&(netlink.NUD_REACHABLE|netlink.NUD_PERMANENT) != 0 {
			return n.HardwareAddr.String(), nil
		}
	}
	return "", nil
}

func toPrefix(n *net.IPNet) (netip.Prefix, bool) {
	if n == nil {
		return netip.Prefix{}, false
	}
	a, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	a = a.Unmap()
	if !a.Is4() {
		return netip.Prefix{}, false
	}
	ones, _ := n.Mask.Size()
	return netip.PrefixFrom(a, ones), true
}
