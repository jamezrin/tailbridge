package nft

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/jamezrin/tailbridge/internal/config"
)

func TestRender(t *testing.T) {
	r := &Rules{
		TSInterface: "tailscale0",
		LAN: []LANRule{{
			Name: "b", Interface: "eth0",
			Alias:   netip.MustParseAddr("192.168.1.100"),
			Peer:    netip.MustParseAddr("100.101.102.103"),
			Sources: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")},
			Ports:   []config.PortRange{{Proto: "tcp", From: 22, To: 22}, {Proto: "udp", From: 5000, To: 5010}, {Proto: "tcp", From: 80, To: 80}},
		}},
		Tailnet: []TailnetRule{
			{Name: "p", Interface: "eth0", VIP: netip.MustParseAddr("10.250.0.10"), Target: netip.MustParseAddr("192.168.1.80"), SNAT: true},
			{Name: "q", Interface: "eth1", VIP: netip.MustParseAddr("10.250.0.11"), Target: netip.MustParseAddr("192.168.2.80")},
		},
	}
	text, hash := Render(r)
	for _, want := range []string{
		`comment "tailbridge sha256:` + hash + `"`,
		`iifname "eth0" ip daddr 192.168.1.100 ip saddr { 192.168.1.0/24 } tcp dport { 22, 80 } dnat to 100.101.102.103 comment "lan:b"`,
		`iifname "eth0" ip daddr 192.168.1.100 ip saddr { 192.168.1.0/24 } udp dport { 5000-5010 } dnat to 100.101.102.103`,
		`oifname "tailscale0" ct status dnat ct original ip daddr 192.168.1.100 masquerade`,
		`ip daddr 192.168.1.100 counter drop`,
		`iifname "tailscale0" ip daddr 10.250.0.10 dnat to 192.168.1.80 comment "tailnet:p"`,
		`oifname "eth0" ct status dnat ct original ip daddr 10.250.0.10 masquerade`,
		`ip daddr 10.250.0.10 counter drop`,
		`ip daddr 10.250.0.11 counter drop`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered table missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "ip daddr 10.250.0.11 masquerade") || strings.Contains(text, `oifname "eth1"`) {
		t.Errorf("SNAT rendered for a no-SNAT mapping:\n%s", text)
	}
	text2, hash2 := Render(r)
	if text2 != text || hash2 != hash {
		t.Error("Render is not deterministic")
	}
	r.Tailnet[0].SNAT = false
	if _, hash3 := Render(r); hash3 == hash {
		t.Error("hash did not change with the rules")
	}
}
