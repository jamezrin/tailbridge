// Package config defines Tailbridge's desired mapping configuration and how
// it is persisted on disk.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Kind identifies the direction of a mapping.
type Kind string

const (
	// KindLAN exposes a tailnet peer on the LAN through a local IPv4 alias
	// (spec feature A).
	KindLAN Kind = "lan"
	// KindTailnet exposes a LAN device to the tailnet through an advertised
	// virtual /32 route (spec feature B).
	KindTailnet Kind = "tailnet"
)

// DefaultTailscaleInterface is the interface tailscaled creates on Linux.
const DefaultTailscaleInterface = "tailscale0"

// CurrentVersion is the config file format version.
const CurrentVersion = 1

// Config is the full set of desired mappings for one gateway.
type Config struct {
	Version int `json:"version"`
	// TailscaleInterface overrides the name of tailscaled's TUN interface.
	TailscaleInterface string    `json:"tailscaleInterface,omitempty"`
	Mappings           []Mapping `json:"mappings"`
}

// Mapping is one exposed device.
type Mapping struct {
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	// Interface is the LAN interface the alias lives on (lan) or the target
	// is reached through (tailnet).
	Interface string `json:"interface"`
	// IP is the LAN alias (lan) or the virtual tailnet-routed address (tailnet).
	IP netip.Addr `json:"ip"`
	// Peer is the tailnet peer to forward to: a hostname, MagicDNS name or
	// Tailscale IPv4 address (lan only).
	Peer string `json:"peer,omitempty"`
	// Target is the real LAN address of the device (tailnet only).
	Target netip.Addr `json:"target,omitzero"`
	// AllowSources restricts which source addresses may use the mapping.
	// Empty means the LAN interface's connected subnets for lan mappings and
	// any tailnet source for tailnet mappings.
	AllowSources []netip.Prefix `json:"allowSources,omitempty"`
	// Ports restricts translated traffic to these protocol/port ranges.
	// Empty means all IPv4 traffic (TCP, UDP, ICMP, ...).
	Ports []PortRange `json:"ports,omitempty"`
	// NoSNAT disables source NAT on the LAN interface (tailnet only). The
	// target must then route replies for tailnet addresses via the gateway.
	NoSNAT bool `json:"noSNAT,omitempty"`
}

// PortRange is a protocol and an inclusive destination port range.
type PortRange struct {
	Proto string // "tcp" or "udp"
	From  uint16
	To    uint16
}

// ParsePortRange parses "tcp:22", "udp:5000-5010" or "tcp:80".
func ParsePortRange(s string) (PortRange, error) {
	proto, ports, ok := strings.Cut(strings.ToLower(strings.TrimSpace(s)), ":")
	if !ok {
		return PortRange{}, fmt.Errorf("port %q: want PROTO:PORT or PROTO:FROM-TO (e.g. tcp:22)", s)
	}
	if proto != "tcp" && proto != "udp" {
		return PortRange{}, fmt.Errorf("port %q: protocol must be tcp or udp", s)
	}
	fromS, toS, isRange := strings.Cut(ports, "-")
	if !isRange {
		toS = fromS
	}
	from, err := parsePort(fromS)
	if err != nil {
		return PortRange{}, fmt.Errorf("port %q: %w", s, err)
	}
	to, err := parsePort(toS)
	if err != nil {
		return PortRange{}, fmt.Errorf("port %q: %w", s, err)
	}
	if to < from {
		return PortRange{}, fmt.Errorf("port %q: range end before start", s)
	}
	return PortRange{Proto: proto, From: from, To: to}, nil
}

func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return uint16(n), nil
}

func (p PortRange) String() string {
	if p.From == p.To {
		return fmt.Sprintf("%s:%d", p.Proto, p.From)
	}
	return fmt.Sprintf("%s:%d-%d", p.Proto, p.From, p.To)
}

// MarshalText implements encoding.TextMarshaler.
func (p PortRange) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (p *PortRange) UnmarshalText(b []byte) error {
	v, err := ParsePortRange(string(b))
	if err != nil {
		return err
	}
	*p = v
	return nil
}

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

var (
	cgnat     = netip.MustParsePrefix("100.64.0.0/10")
	loopback  = netip.MustParsePrefix("127.0.0.0/8")
	linkLocal = netip.MustParsePrefix("169.254.0.0/16")
	multicast = netip.MustParsePrefix("224.0.0.0/4")
)

// unusableAddr reports why an address can never be used as an alias or
// virtual IP, or "" if it is acceptable.
func unusableAddr(a netip.Addr) string {
	switch {
	case !a.IsValid():
		return "is missing"
	case !a.Is4():
		return "must be IPv4 (IPv6 is not supported in v1)"
	case a.IsUnspecified(), a == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return "is not a unicast address"
	case loopback.Contains(a), linkLocal.Contains(a), multicast.Contains(a):
		return "is a loopback, link-local or multicast address"
	case cgnat.Contains(a):
		return "is inside Tailscale's 100.64.0.0/10 range"
	}
	return ""
}

// Validate checks a single mapping in isolation.
func (m *Mapping) Validate() error {
	if !nameRE.MatchString(m.Name) {
		return fmt.Errorf("invalid mapping name %q: use letters, digits, '.', '_' or '-'", m.Name)
	}
	if m.Interface == "" {
		return fmt.Errorf("mapping %q: interface is required", m.Name)
	}
	if why := unusableAddr(m.IP); why != "" {
		return fmt.Errorf("mapping %q: ip %s", m.Name, why)
	}
	for _, p := range m.AllowSources {
		if !p.IsValid() || !p.Addr().Is4() {
			return fmt.Errorf("mapping %q: allowed source %s must be an IPv4 prefix", m.Name, p)
		}
	}
	switch m.Kind {
	case KindLAN:
		if m.Peer == "" {
			return fmt.Errorf("mapping %q: peer is required", m.Name)
		}
		if m.Target.IsValid() {
			return fmt.Errorf("mapping %q: target is only valid for tailnet mappings", m.Name)
		}
		if m.NoSNAT {
			return fmt.Errorf("mapping %q: noSNAT is only valid for tailnet mappings (lan mappings always masquerade toward the tailnet)", m.Name)
		}
	case KindTailnet:
		if m.Peer != "" {
			return fmt.Errorf("mapping %q: peer is only valid for lan mappings", m.Name)
		}
		if !m.Target.IsValid() || !m.Target.Is4() {
			return fmt.Errorf("mapping %q: target must be an IPv4 address", m.Name)
		}
		if m.Target == m.IP {
			return fmt.Errorf("mapping %q: virtual ip and target must differ", m.Name)
		}
	default:
		return fmt.Errorf("mapping %q: unknown kind %q", m.Name, m.Kind)
	}
	return nil
}

// Validate checks every mapping and cross-mapping uniqueness.
func (c *Config) Validate() error {
	names := map[string]bool{}
	ips := map[netip.Addr]string{}
	for i := range c.Mappings {
		m := &c.Mappings[i]
		if err := m.Validate(); err != nil {
			return err
		}
		if names[m.Name] {
			return fmt.Errorf("duplicate mapping name %q", m.Name)
		}
		names[m.Name] = true
		if other, ok := ips[m.IP]; ok {
			return fmt.Errorf("mapping %q: ip %s is already used by mapping %q", m.Name, m.IP, other)
		}
		ips[m.IP] = m.Name
	}
	for i := range c.Mappings {
		m := &c.Mappings[i]
		if m.Kind == KindTailnet {
			if other, ok := ips[m.Target]; ok {
				return fmt.Errorf("mapping %q: target %s is a virtual address of mapping %q", m.Name, m.Target, other)
			}
		}
	}
	return nil
}

// TSInterface returns the configured tailscale interface name.
func (c *Config) TSInterface() string {
	if c.TailscaleInterface != "" {
		return c.TailscaleInterface
	}
	return DefaultTailscaleInterface
}

// Find returns the mapping with the given name.
func (c *Config) Find(name string) (*Mapping, bool) {
	i := slices.IndexFunc(c.Mappings, func(m Mapping) bool { return m.Name == name })
	if i < 0 {
		return nil, false
	}
	return &c.Mappings[i], true
}

// Remove deletes the mapping with the given name, reporting whether it existed.
func (c *Config) Remove(name string) bool {
	n := len(c.Mappings)
	c.Mappings = slices.DeleteFunc(c.Mappings, func(m Mapping) bool { return m.Name == name })
	return len(c.Mappings) != n
}

// Clone returns a deep copy.
func (c *Config) Clone() *Config {
	b, _ := json.Marshal(c)
	var out Config
	_ = json.Unmarshal(b, &out)
	return &out
}

// Load reads a config file. A missing file yields an empty config.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{Version: CurrentVersion}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Version != CurrentVersion {
		return nil, fmt.Errorf("%s: unsupported config version %d", path, c.Version)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Save validates and atomically writes the config.
func Save(path string, c *Config) error {
	c.Version = CurrentVersion
	if c.Mappings == nil {
		c.Mappings = []Mapping{}
	}
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(b, '\n'), 0o644)
}

// WriteFileAtomic writes data to a temp file in the same directory and
// renames it into place.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
