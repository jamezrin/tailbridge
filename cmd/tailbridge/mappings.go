package main

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jamezrin/tailbridge/internal/config"
	"github.com/jamezrin/tailbridge/internal/engine"
	"github.com/jamezrin/tailbridge/internal/netif"
	"github.com/jamezrin/tailbridge/internal/state"
)

type commonAddFlags struct {
	ip        string
	iface     string
	allowSrc  []string
	ports     []string
	force     bool
	dryRun    bool
	showRules bool
}

func (f *commonAddFlags) register(cmd *cobra.Command, ipHelp string) {
	fl := cmd.Flags()
	fl.StringVar(&f.ip, "ip", "", ipHelp)
	fl.StringVar(&f.iface, "interface", "", "LAN interface (e.g. eth0)")
	fl.StringSliceVar(&f.allowSrc, "allow-src", nil, "allowed source IPv4 prefixes (repeatable; \"any\" for 0.0.0.0/0)")
	fl.StringSliceVar(&f.ports, "port", nil, "only translate these ports, e.g. tcp:22, udp:5000-5010 (repeatable; default all traffic)")
	fl.BoolVar(&f.force, "force", false, "skip soft pre-flight checks (alias outside subnet, ARP answer)")
	fl.BoolVar(&f.dryRun, "dry-run", false, "show what would change without changing anything")
	fl.BoolVar(&f.showRules, "show-rules", false, "with --dry-run, print the full nftables table")
	_ = cmd.MarkFlagRequired("ip")
	_ = cmd.MarkFlagRequired("interface")
}

func (f *commonAddFlags) fill(m *config.Mapping) error {
	ip, err := netip.ParseAddr(f.ip)
	if err != nil {
		return fmt.Errorf("--ip: %w", err)
	}
	m.IP, m.Interface = ip, f.iface
	for _, s := range f.allowSrc {
		if s == "any" {
			s = "0.0.0.0/0"
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil {
				return fmt.Errorf("--allow-src %q: %w", s, err)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		m.AllowSources = append(m.AllowSources, p.Masked())
	}
	for _, s := range f.ports {
		p, err := config.ParsePortRange(s)
		if err != nil {
			return err
		}
		m.Ports = append(m.Ports, p)
	}
	return nil
}

func lanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lan",
		Short: "Expose tailnet peers on the LAN through local IPv4 aliases (feature A)",
	}
	var f commonAddFlags
	var name string
	add := &cobra.Command{
		Use:   "add PEER --ip ALIAS --interface IFACE",
		Short: "Give a tailnet peer a LAN IPv4 alias on this gateway",
		Long: `Claim ALIAS on IFACE and forward traffic for it to PEER on the tailnet.

PEER is a tailnet hostname, MagicDNS name or Tailscale IPv4 address and is
re-resolved continuously. By default only the interface's connected subnets
may use the alias.

Connections reach PEER from this gateway's Tailscale IP and identity: tailnet
ACLs apply to the gateway, not to individual LAN clients.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m := config.Mapping{Kind: config.KindLAN, Peer: args[0], Name: name}
			if m.Name == "" {
				m.Name = sanitizeName(args[0])
			}
			if err := f.fill(&m); err != nil {
				return err
			}
			return addMapping(cmd.Context(), m, &f)
		},
	}
	add.Flags().StringVar(&name, "name", "", "mapping name (default: derived from PEER)")
	f.register(add, "LAN IPv4 alias to claim on the interface")
	cmd.AddCommand(add, removeCmd())
	return cmd
}

func tailnetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tailnet",
		Short: "Expose LAN devices to the tailnet through advertised /32 routes (feature B)",
	}
	var f commonAddFlags
	var target string
	var noSNAT bool
	add := &cobra.Command{
		Use:   "add NAME --target LAN-IP --ip VIRTUAL-IP --interface IFACE",
		Short: "Give a LAN device a virtual IPv4 address reachable from the tailnet",
		Long: `Advertise VIRTUAL-IP/32 through tailscaled and translate it to LAN-IP via IFACE.

The route must be approved in the Tailscale admin console (or by
autoApprovers), and clients that require it must accept routes
(tailscale set --accept-routes on Linux). VIRTUAL-IP is an ordinary routed
private address, not a Tailscale node IP. Inbound connections only.

By default connections are source-NATed to this gateway's LAN address so the
device's replies return through the gateway.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := netip.ParseAddr(target)
			if err != nil {
				return fmt.Errorf("--target: %w", err)
			}
			m := config.Mapping{Kind: config.KindTailnet, Name: args[0], Target: t, NoSNAT: noSNAT}
			if err := f.fill(&m); err != nil {
				return err
			}
			return addMapping(cmd.Context(), m, &f)
		},
	}
	add.Flags().StringVar(&target, "target", "", "real LAN IPv4 address of the device")
	add.Flags().BoolVar(&noSNAT, "no-snat", false, "do not source-NAT on the LAN interface (device must route tailnet replies via this gateway)")
	_ = add.MarkFlagRequired("target")
	f.register(add, "virtual IPv4 address to advertise as a /32 route")
	cmd.AddCommand(add, removeCmd())
	return cmd
}

func sanitizeName(s string) string {
	s, _, _ = strings.Cut(strings.TrimSuffix(s, "."), ".")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// preflight runs soft checks that --force may skip.
func preflight(m *config.Mapping, st *state.State) error {
	if m.Kind != config.KindLAN {
		return nil
	}
	var nl netif.Netlink
	if !nl.LinkExists(m.Interface) {
		return nil // reported by reconcile
	}
	subnets, err := nl.Subnets(m.Interface)
	if err != nil {
		return err
	}
	inside := false
	for _, p := range subnets {
		inside = inside || p.Contains(m.IP)
	}
	if !inside {
		return fmt.Errorf("alias %s is outside the connected subnets of %s; LAN clients could not reach it without extra routes (use --force to add anyway)", m.IP, m.Interface)
	}
	if owner, _ := nl.AddrOwner(m.IP); owner == "" && !st.HasAddr(m.Interface, m.IP) {
		mac, err := netif.ProbeARP(m.Interface, m.IP, 600*time.Millisecond)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: ARP probe skipped: %v\n", err)
		} else if mac != "" {
			return fmt.Errorf("another LAN host (%s) answers ARP for %s (use --force to add anyway)", mac, m.IP)
		}
	}
	return nil
}

// addMapping adds m transactionally: if the new mapping cannot be made
// active, the previous configuration is restored and reconciled again.
func addMapping(ctx context.Context, m config.Mapping, f *commonAddFlags) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if !f.dryRun {
		if err := requireRoot(); err != nil {
			return err
		}
	}
	return withLock(func(cfg *config.Config) error {
		if _, ok := cfg.Find(m.Name); ok {
			return fmt.Errorf("mapping %q already exists; remove it first", m.Name)
		}
		next := cfg.Clone()
		next.Mappings = append(next.Mappings, m)
		if err := next.Validate(); err != nil {
			return err
		}
		eng := newEngine()
		if !f.force {
			st, err := eng.State.Load()
			if err != nil {
				return err
			}
			if err := preflight(&m, st); err != nil {
				return err
			}
		}
		if f.dryRun {
			res, err := eng.Reconcile(ctx, next, engine.Options{DryRun: true})
			if err != nil {
				return err
			}
			printResult(res, true, f.showRules)
			if ms := res.Mapping(m.Name); ms != nil && ms.State == engine.StateError {
				return fmt.Errorf("mapping %q would fail: %s", m.Name, strings.Join(ms.Errors, "; "))
			}
			return nil
		}

		if err := config.Save(g.configPath, next); err != nil {
			return err
		}
		res, err := eng.Reconcile(ctx, next, engine.Options{})
		ms := res.Mapping(m.Name)
		if ms == nil || ms.State == engine.StateError {
			reason := "reconcile failed"
			if ms != nil && len(ms.Errors) > 0 {
				reason = strings.Join(ms.Errors, "; ")
			} else if err != nil {
				reason = err.Error()
			}
			if serr := config.Save(g.configPath, cfg); serr != nil {
				return fmt.Errorf("%s; additionally failed to restore previous config: %w", reason, serr)
			}
			if _, rerr := eng.Reconcile(ctx, cfg, engine.Options{}); rerr != nil {
				return fmt.Errorf("%s; rollback reconcile also failed: %w", reason, rerr)
			}
			return fmt.Errorf("mapping %q not added: %s", m.Name, reason)
		}
		if g.jsonOut {
			return printJSON(ms)
		}
		printResult(res, false, false)
		switch m.Kind {
		case config.KindLAN:
			fmt.Printf("\nNote: LAN clients reach %s through this gateway's Tailscale identity; tailnet ACLs\napply to the gateway, not to individual LAN clients. Allowed sources: %s\n", ms.PeerName, joinPrefixes(ms.Sources))
		case config.KindTailnet:
			approval := ""
			if ms.Route != "approved" {
				approval = fmt.Sprintf("%s/32 must be approved for this node (admin console or autoApprovers). ", m.IP)
			}
			fmt.Printf("\nNote: %sLinux clients need --accept-routes.\n%s\n", approval, snatNote(m))
		}
		return err
	})
}

func snatNote(m config.Mapping) string {
	if m.NoSNAT {
		return fmt.Sprintf("SNAT disabled: %s must route tailnet replies via this gateway.", m.Target)
	}
	return fmt.Sprintf("%s will see connections from this gateway's LAN address.", m.Target)
}

func removeCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:     "remove NAME",
		Aliases: []string{"rm"},
		Short:   "Remove a mapping and clean up only its Tailbridge-owned state",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !dryRun {
				if err := requireRoot(); err != nil {
					return err
				}
			}
			return withLock(func(cfg *config.Config) error {
				next := cfg.Clone()
				if !next.Remove(args[0]) {
					return fmt.Errorf("no mapping named %q", args[0])
				}
				eng := newEngine()
				if dryRun {
					res, err := eng.Reconcile(cmd.Context(), next, engine.Options{DryRun: true})
					if err != nil {
						return err
					}
					printPlan(&res.Plan, true, false)
					return nil
				}
				if err := config.Save(g.configPath, next); err != nil {
					return err
				}
				res, err := eng.Reconcile(cmd.Context(), next, engine.Options{})
				if res != nil && !g.jsonOut {
					printPlan(&res.Plan, false, false)
				}
				if err != nil {
					return err
				}
				fmt.Printf("removed %s\n", args[0])
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would change without changing anything")
	return cmd
}

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configured mappings (no live state; see status)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(g.configPath)
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cfg.Mappings)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tKIND\tINTERFACE\tADDRESS\tFORWARDS TO\tALLOWED SOURCES\tPORTS")
			for _, m := range cfg.Mappings {
				dst := m.Peer
				if m.Kind == config.KindTailnet {
					dst = m.Target.String()
				}
				src := joinPrefixes(m.AllowSources)
				if len(m.AllowSources) == 0 {
					src = map[config.Kind]string{config.KindLAN: "(interface subnets)", config.KindTailnet: "(any)"}[m.Kind]
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", m.Name, m.Kind, m.Interface, m.IP, dst, src, joinPorts(m.Ports))
			}
			return tw.Flush()
		},
	}
}

func joinPrefixes(ps []netip.Prefix) string {
	if len(ps) == 0 {
		return "any"
	}
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = p.String()
	}
	return strings.Join(s, ",")
}

func joinPorts(ps []config.PortRange) string {
	if len(ps) == 0 {
		return "all"
	}
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = p.String()
	}
	return strings.Join(s, ",")
}
