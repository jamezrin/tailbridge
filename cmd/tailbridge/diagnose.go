package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"tailscale.com/types/preftype"

	"github.com/jamezrin/tailbridge/internal/config"
	"github.com/jamezrin/tailbridge/internal/engine"
	"github.com/jamezrin/tailbridge/internal/nft"
	"github.com/jamezrin/tailbridge/internal/sysctl"
)

type check struct {
	Level   string `json:"level"` // ok, info, warn, fail
	Subject string `json:"subject"`
	Message string `json:"message"`
}

func diagnoseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diagnose",
		Short: "Check privileges, dependencies, firewall interplay and every mapping",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var checks []check
			add := func(level, subject, format string, a ...any) {
				checks = append(checks, check{level, subject, fmt.Sprintf(format, a...)})
			}

			if os.Geteuid() == 0 {
				add("ok", "privileges", "running as root")
			} else {
				add("fail", "privileges", "not running as root; tailbridge needs root (CAP_NET_ADMIN) to inspect and change networking")
			}

			nftExec := nft.Exec{Path: g.nftPath}
			if v, err := nftExec.Available(); err != nil {
				add("fail", "nftables", "nft is not usable: %v", err)
			} else {
				add("ok", "nftables", "%s", v)
			}

			cfg, err := config.Load(g.configPath)
			if err != nil {
				add("fail", "config", "%v", err)
				return report(checks)
			}
			add("ok", "config", "%s: %d mapping(s)", g.configPath, len(cfg.Mappings))

			if on, err := (sysctl.Proc{}).Enabled(); err != nil {
				add("warn", "ip_forward", "cannot read: %v", err)
			} else if on {
				add("ok", "ip_forward", "net.ipv4.ip_forward=1")
			} else if len(cfg.Mappings) > 0 {
				add("fail", "ip_forward", "net.ipv4.ip_forward=0; `tailbridge apply` will enable it")
			} else {
				add("info", "ip_forward", "net.ipv4.ip_forward=0 (not needed without mappings)")
			}

			eng := newEngine()
			ctx := cmd.Context()
			if st, err := eng.TS.Status(ctx); err != nil {
				add("fail", "tailscaled", "Local API at %s unreachable: %v", g.socket, err)
			} else {
				lvl := "ok"
				if st.BackendState != "Running" {
					lvl = "fail"
				}
				self := "unknown"
				if st.Self != nil {
					self = fmt.Sprintf("%s %v", strings.TrimSuffix(st.Self.DNSName, "."), st.Self.TailscaleIPs)
				}
				add(lvl, "tailscaled", "backend %s, this node: %s", st.BackendState, self)
				if prefs, err := eng.TS.GetPrefs(ctx); err == nil {
					if prefs.NetfilterMode == preftype.NetfilterOff {
						add("warn", "tailscaled", "netfilter mode is off: tailscaled installs no forwarding rules, so make sure your firewall forwards between %s and the LAN interfaces", cfg.TSInterface())
					}
					if prefs.RouteAll {
						add("info", "tailscaled", "this gateway accepts routes; tailbridge checks that mapped targets still route via their LAN interface")
					}
				}
			}
			if eng.Net.LinkExists(cfg.TSInterface()) {
				add("ok", "interface", "%s exists", cfg.TSInterface())
			} else {
				add("fail", "interface", "%s does not exist (tailscaled in userspace-networking mode is not supported)", cfg.TSInterface())
			}

			diagnoseForward(nftExec, add)

			res, err := eng.Reconcile(ctx, cfg, engine.Options{DryRun: true})
			if err != nil {
				add("fail", "reconcile", "%v", err)
				return report(checks)
			}
			for _, w := range res.Warnings {
				add("warn", "reconcile", "%s", w)
			}
			if steps := res.Plan.Steps(); len(steps) > 0 {
				add("warn", "drift", "host differs from configuration; `tailbridge apply` would: %s", strings.Join(steps, "; "))
			} else if len(cfg.Mappings) > 0 {
				add("ok", "drift", "aliases, firewall table and advertised routes match the configuration")
			}
			var hasLAN, hasTailnet bool
			for _, m := range res.Mappings {
				subject := "mapping " + m.Name
				for _, e := range m.Errors {
					add("fail", subject, "%s", e)
				}
				for _, w := range m.Warnings {
					add("warn", subject, "%s", w)
				}
				if len(m.Errors) == 0 && len(m.Warnings) == 0 {
					add("ok", subject, "%s", m.State)
				}
				hasLAN = hasLAN || m.Kind == config.KindLAN
				hasTailnet = hasTailnet || m.Kind == config.KindTailnet
			}
			if hasLAN {
				add("info", "acl", "lan mappings: the tailnet policy must allow this gateway to reach each peer on the used ports; peers see the gateway's identity, not LAN clients")
			}
			if hasTailnet {
				add("info", "acl", "tailnet mappings: the policy must allow clients to reach each virtual /32, routes must be approved (or autoApprovers), and Linux clients need --accept-routes")
			}
			return report(checks)
		},
	}
}

// diagnoseForward warns when forwarded traffic is likely to be dropped by
// another firewall (for example Docker's FORWARD policy DROP) without
// Tailscale's own accept rules being present.
func diagnoseForward(e nft.Exec, add func(level, subject, format string, a ...any)) {
	chains, err := e.Chains()
	if err != nil {
		add("warn", "firewall", "cannot list nftables chains: %v", err)
		return
	}
	var dropping []string
	tsForward := false
	for _, c := range chains {
		if c.Hook == "forward" && c.Policy == "drop" {
			dropping = append(dropping, fmt.Sprintf("%s %s %s", c.Family, c.Table, c.Name))
		}
		if c.Name == "ts-forward" {
			tsForward = true
		}
	}
	switch {
	case len(dropping) > 0 && !tsForward:
		add("warn", "firewall", "forward chain(s) with policy drop and no Tailscale ts-forward rules: %s; forwarded mapping traffic will likely be dropped. Enable tailscaled netfilter management or allow forwarding between the LAN and tailscale interfaces", strings.Join(dropping, ", "))
	case len(dropping) > 0:
		add("ok", "firewall", "forward policy drop in %s, but Tailscale's ts-forward rules accept tailnet traffic", strings.Join(dropping, ", "))
	default:
		add("ok", "firewall", "no forward chain drops by default")
	}
	if b, err := os.ReadFile("/proc/net/ip_tables_names"); err == nil && strings.TrimSpace(string(b)) != "" {
		add("warn", "firewall", "legacy iptables tables are loaded (%s); their rules are invisible to nft and may drop forwarded traffic", strings.Join(strings.Fields(string(b)), ", "))
	}
}

func report(checks []check) error {
	failed := false
	for _, c := range checks {
		failed = failed || c.Level == "fail"
	}
	if g.jsonOut {
		_ = printJSON(checks)
	} else {
		for _, c := range checks {
			fmt.Printf("[%-4s] %-18s %s\n", strings.ToUpper(c.Level), c.Subject, c.Message)
		}
	}
	if failed {
		return errors.New("diagnose found problems")
	}
	return nil
}
