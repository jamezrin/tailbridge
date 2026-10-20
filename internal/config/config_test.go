package config

import (
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePortRange(t *testing.T) {
	for in, want := range map[string]PortRange{
		"tcp:22":        {"tcp", 22, 22},
		"UDP:5000-5010": {"udp", 5000, 5010},
	} {
		got, err := ParsePortRange(in)
		if err != nil || got != want {
			t.Errorf("ParsePortRange(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"22", "icmp:1", "tcp:0", "tcp:70000", "tcp:10-5", "tcp:x"} {
		if _, err := ParsePortRange(in); err == nil {
			t.Errorf("ParsePortRange(%q) succeeded", in)
		}
	}
}

func lan(name, ip string) Mapping {
	return Mapping{Name: name, Kind: KindLAN, Interface: "eth0", IP: netip.MustParseAddr(ip), Peer: "device-b"}
}

func tailnet(name, ip, target string) Mapping {
	return Mapping{Name: name, Kind: KindTailnet, Interface: "eth0", IP: netip.MustParseAddr(ip), Target: netip.MustParseAddr(target)}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"ok", Config{Mappings: []Mapping{lan("a", "192.168.1.100"), tailnet("b", "10.250.0.10", "192.168.1.80")}}, ""},
		{"dup name", Config{Mappings: []Mapping{lan("a", "192.168.1.100"), lan("a", "192.168.1.101")}}, "duplicate mapping name"},
		{"dup ip", Config{Mappings: []Mapping{lan("a", "192.168.1.100"), tailnet("b", "192.168.1.100", "192.168.1.80")}}, "already used"},
		{"cgnat", Config{Mappings: []Mapping{lan("a", "100.100.1.1")}}, "100.64.0.0/10"},
		{"ipv6", Config{Mappings: []Mapping{lan("a", "fd00::1")}}, "IPv4"},
		{"bad name", Config{Mappings: []Mapping{lan("a b", "192.168.1.100")}}, "invalid mapping name"},
		{"target is vip", Config{Mappings: []Mapping{tailnet("a", "10.250.0.10", "10.250.0.11"), tailnet("b", "10.250.0.11", "192.168.1.80")}}, "virtual address of mapping"},
		{"lan nosnat", Config{Mappings: []Mapping{func() Mapping { m := lan("a", "192.168.1.100"); m.NoSNAT = true; return m }()}}, "noSNAT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	empty, err := Load(path)
	if err != nil || len(empty.Mappings) != 0 {
		t.Fatalf("Load(missing) = %v, %v", empty, err)
	}
	m := lan("a", "192.168.1.100")
	m.AllowSources = []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}
	m.Ports = []PortRange{{"tcp", 22, 22}, {"udp", 5000, 5010}}
	in := &Config{Mappings: []Mapping{m, tailnet("b", "10.250.0.10", "192.168.1.80")}}
	if err := Save(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Mappings[0].Ports[1].String(); got != "udp:5000-5010" {
		t.Errorf("port round trip = %q", got)
	}
	if out.Mappings[1].Target != netip.MustParseAddr("192.168.1.80") || out.Mappings[0].Target.IsValid() {
		t.Errorf("target round trip: %+v", out.Mappings)
	}
	if out.TSInterface() != DefaultTailscaleInterface {
		t.Errorf("TSInterface = %q", out.TSInterface())
	}
}
