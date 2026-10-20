// Package nft renders and applies Tailbridge's dedicated nftables table.
//
// Tailbridge owns exactly one table, "ip tailbridge", and always replaces it
// atomically. It never flushes the ruleset or edits tables owned by
// tailscaled, Docker, firewalld or the administrator.
package nft

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
	"strings"

	"github.com/jamezrin/tailbridge/internal/config"
)

// Family and Table identify the Tailbridge-owned table.
const (
	Family = "ip"
	Table  = "tailbridge"
)

const hashPrefix = "tailbridge sha256:"

// LANRule forwards a LAN alias to a tailnet peer (feature A).
type LANRule struct {
	Name      string
	Interface string
	Alias     netip.Addr
	Peer      netip.Addr
	Sources   []netip.Prefix
	Ports     []config.PortRange
}

// TailnetRule forwards a tailnet virtual IP to a LAN device (feature B).
type TailnetRule struct {
	Name      string
	Interface string
	VIP       netip.Addr
	Target    netip.Addr
	Sources   []netip.Prefix // empty: any
	Ports     []config.PortRange
	SNAT      bool
}

// Rules is the complete desired Tailbridge firewall state.
type Rules struct {
	TSInterface string
	LAN         []LANRule
	Tailnet     []TailnetRule
}

// Empty reports whether no rules are needed, in which case the table should
// not exist at all.
func (r *Rules) Empty() bool { return len(r.LAN) == 0 && len(r.Tailnet) == 0 }

// Render returns the table definition and a hash of it. The hash is stored
// as the table comment so reconciliation can detect drift cheaply.
func Render(r *Rules) (text, hash string) {
	var body strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&body, format, args...) }

	w("\tchain prerouting {\n\t\ttype nat hook prerouting priority -110; policy accept;\n")
	for _, m := range r.LAN {
		for _, match := range portMatches(m.Ports) {
			w("\t\tiifname %q ip daddr %s%s%s dnat to %s comment %q\n",
				m.Interface, m.Alias, saddrMatch(m.Sources), match, m.Peer, "lan:"+m.Name)
		}
	}
	for _, m := range r.Tailnet {
		for _, match := range portMatches(m.Ports) {
			w("\t\tiifname %q ip daddr %s%s%s dnat to %s comment %q\n",
				r.TSInterface, m.VIP, saddrMatch(m.Sources), match, m.Target, "tailnet:"+m.Name)
		}
	}
	w("\t}\n\n")

	w("\tchain postrouting {\n\t\ttype nat hook postrouting priority 90; policy accept;\n")
	for _, m := range r.LAN {
		w("\t\toifname %q ct status dnat ct original ip daddr %s masquerade comment %q\n",
			r.TSInterface, m.Alias, "lan:"+m.Name)
	}
	for _, m := range r.Tailnet {
		if m.SNAT {
			w("\t\toifname %q ct status dnat ct original ip daddr %s masquerade comment %q\n",
				m.Interface, m.VIP, "tailnet:"+m.Name)
		}
	}
	w("\t}\n\n")

	// Traffic to a LAN alias that was not translated (disallowed source or
	// port) must not reach the gateway's own services.
	w("\tchain input {\n\t\ttype filter hook input priority -10; policy accept;\n")
	for _, m := range r.LAN {
		w("\t\tip daddr %s counter drop comment %q\n", m.Alias, "lan:"+m.Name+":untranslated")
	}
	w("\t}\n\n")

	// Traffic to a virtual IP that was not translated must not be routed
	// anywhere else (e.g. out of the default route).
	w("\tchain forward {\n\t\ttype filter hook forward priority -10; policy accept;\n")
	for _, m := range r.Tailnet {
		w("\t\tip daddr %s counter drop comment %q\n", m.VIP, "tailnet:"+m.Name+":untranslated")
	}
	w("\t}\n")

	sum := sha256.Sum256([]byte(body.String()))
	hash = hex.EncodeToString(sum[:8])
	text = fmt.Sprintf("table %s %s {\n\tcomment %q\n\n%s}\n", Family, Table, hashPrefix+hash, body.String())
	return text, hash
}

func saddrMatch(srcs []netip.Prefix) string {
	if len(srcs) == 0 {
		return ""
	}
	parts := make([]string, len(srcs))
	for i, p := range srcs {
		parts[i] = p.String()
	}
	slices.Sort(parts)
	parts = slices.Compact(parts)
	return " ip saddr { " + strings.Join(parts, ", ") + " }"
}

// portMatches returns one match expression per protocol, or a single empty
// match (all traffic) when no ports are configured.
func portMatches(ports []config.PortRange) []string {
	if len(ports) == 0 {
		return []string{""}
	}
	byProto := map[string][]string{}
	for _, p := range ports {
		v := fmt.Sprint(p.From)
		if p.To != p.From {
			v = fmt.Sprintf("%d-%d", p.From, p.To)
		}
		byProto[p.Proto] = append(byProto[p.Proto], v)
	}
	var out []string
	for _, proto := range []string{"tcp", "udp"} {
		if vs := byProto[proto]; len(vs) > 0 {
			out = append(out, fmt.Sprintf(" %s dport { %s }", proto, strings.Join(vs, ", ")))
		}
	}
	return out
}

// Chain describes a base or regular chain found in the live ruleset.
type Chain struct {
	Family string `json:"family"`
	Table  string `json:"table"`
	Name   string `json:"name"`
	Hook   string `json:"hook"`
	Policy string `json:"policy"`
}

// Backend applies rules to the kernel.
type Backend interface {
	// CurrentHash returns the hash comment of the live table, if present.
	CurrentHash() (hash string, present bool, err error)
	// Replace atomically replaces the Tailbridge table with text.
	Replace(text string) error
	// Delete removes the Tailbridge table if it exists.
	Delete() error
	// Chains lists all chains in the live ruleset.
	Chains() ([]Chain, error)
}

// Exec drives the nft command-line tool.
type Exec struct {
	// Path is the nft binary; defaults to "nft".
	Path string
}

var _ Backend = Exec{}

func (e Exec) bin() string {
	if e.Path != "" {
		return e.Path
	}
	return "nft"
}

func (e Exec) run(stdin string, args ...string) ([]byte, error) {
	cmd := exec.Command(e.bin(), args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("nft %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

type jsonRuleset struct {
	Nftables []map[string]json.RawMessage `json:"nftables"`
}

func (e Exec) CurrentHash() (string, bool, error) {
	out, err := e.run("", "-j", "list", "tables")
	if err != nil {
		return "", false, err
	}
	var rs jsonRuleset
	if err := json.Unmarshal(out, &rs); err != nil {
		return "", false, fmt.Errorf("parse nft tables: %w", err)
	}
	for _, obj := range rs.Nftables {
		raw, ok := obj["table"]
		if !ok {
			continue
		}
		var t struct {
			Family  string `json:"family"`
			Name    string `json:"name"`
			Comment string `json:"comment"`
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			continue
		}
		if t.Family == Family && t.Name == Table {
			return strings.TrimPrefix(t.Comment, hashPrefix), true, nil
		}
	}
	return "", false, nil
}

func (e Exec) Replace(text string) error {
	// Creating then deleting the table first makes the delete succeed
	// whether or not the table exists; nft -f applies the whole file as a
	// single atomic transaction.
	script := fmt.Sprintf("table %s %s\ndelete table %s %s\n%s", Family, Table, Family, Table, text)
	_, err := e.run(script, "-f", "-")
	return err
}

func (e Exec) Delete() error {
	_, present, err := e.CurrentHash()
	if err != nil || !present {
		return err
	}
	_, err = e.run("", "delete", "table", Family, Table)
	return err
}

func (e Exec) Chains() ([]Chain, error) {
	out, err := e.run("", "-j", "list", "chains")
	if err != nil {
		return nil, err
	}
	var rs jsonRuleset
	if err := json.Unmarshal(out, &rs); err != nil {
		return nil, fmt.Errorf("parse nft chains: %w", err)
	}
	var chains []Chain
	for _, obj := range rs.Nftables {
		if raw, ok := obj["chain"]; ok {
			var c Chain
			if json.Unmarshal(raw, &c) == nil {
				chains = append(chains, c)
			}
		}
	}
	return chains, nil
}

// Available reports whether the nft binary can be executed.
func (e Exec) Available() (string, error) {
	out, err := e.run("", "--version")
	return strings.TrimSpace(string(out)), err
}
