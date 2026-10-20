package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/vishvananda/netlink"

	"github.com/jamezrin/tailbridge/internal/config"
	"github.com/jamezrin/tailbridge/internal/engine"
)

func printPlan(p *engine.Plan, dryRun, showRules bool) {
	steps := p.Steps()
	switch {
	case len(steps) == 0:
		fmt.Println("No changes needed.")
	case dryRun:
		fmt.Println("Would apply:")
	default:
		fmt.Println("Applied:")
	}
	for _, s := range steps {
		fmt.Println("  -", s)
	}
	if showRules && p.Ruleset != "" {
		fmt.Printf("\nnftables table:\n%s", p.Ruleset)
	}
}

func printResult(res *engine.Result, dryRun, showRules bool) {
	if g.jsonOut {
		_ = printJSON(res)
		return
	}
	printPlan(&res.Plan, dryRun, showRules)
	fmt.Println()
	printStatusTable(res)
}

func printStatusTable(res *engine.Result) {
	for _, w := range res.Warnings {
		fmt.Println("warning:", w)
	}
	if len(res.Mappings) == 0 {
		fmt.Println("No mappings configured.")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tADDRESS\tFORWARDS TO\tSTATE\tDETAIL")
	for _, m := range res.Mappings {
		var dst, detail string
		switch m.Kind {
		case config.KindLAN:
			dst = m.Peer
			if m.PeerIP.IsValid() {
				dst = fmt.Sprintf("%s (%s)", m.Peer, m.PeerIP)
			}
			if m.PeerOnline != nil {
				detail = map[bool]string{true: "peer online", false: "peer offline"}[*m.PeerOnline]
			}
		case config.KindTailnet:
			dst = m.Target.String()
			detail = "route " + m.Route
		}
		fmt.Fprintf(tw, "%s\t%s\t%s@%s\t%s\t%s\t%s\n", m.Name, m.Kind, m.IP, m.Interface, dst, m.State, detail)
	}
	_ = tw.Flush()
	for _, m := range res.Mappings {
		for _, e := range m.Errors {
			fmt.Printf("%s: error: %s\n", m.Name, e)
		}
		for _, w := range m.Warnings {
			fmt.Printf("%s: warning: %s\n", m.Name, w)
		}
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show live state of every mapping and any drift from the configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(g.configPath)
			if err != nil {
				return err
			}
			res, err := newEngine().Reconcile(cmd.Context(), cfg, engine.Options{DryRun: true})
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(res)
			}
			printStatusTable(res)
			if steps := res.Plan.Steps(); len(steps) > 0 {
				fmt.Println("\nHost state differs from the configuration; `tailbridge apply` would:")
				for _, s := range steps {
					fmt.Println("  -", s)
				}
			}
			return nil
		},
	}
}

func applyCmd() *cobra.Command {
	var dryRun, showRules bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Reconcile host state with the configuration once",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !dryRun {
				if err := requireRoot(); err != nil {
					return err
				}
			}
			return withLock(func(cfg *config.Config) error {
				res, err := newEngine().Reconcile(cmd.Context(), cfg, engine.Options{DryRun: dryRun})
				if res != nil {
					printResult(res, dryRun, showRules || (dryRun && !g.jsonOut))
				}
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the plan, including the full nftables table, without applying it")
	cmd.Flags().BoolVar(&showRules, "show-rules", false, "print the nftables table")
	return cmd
}

func downCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Remove all Tailbridge-owned aliases, firewall rules and advertised routes",
		Long: `Remove every resource Tailbridge created: its LAN aliases, its nftables table and
the advertised routes it added. Routes and rules owned by anyone else, and
net.ipv4.ip_forward, are left untouched. The configuration is kept, so
'tailbridge apply' restores everything. Stop the daemon first, or it will
re-apply the configuration.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !dryRun {
				if err := requireRoot(); err != nil {
					return err
				}
			}
			unlock, err := lockState()
			if err != nil {
				return err
			}
			defer unlock()
			p, err := newEngine().Down(cmd.Context(), dryRun)
			if p != nil {
				if g.jsonOut {
					_ = printJSON(p)
				} else {
					printPlan(p, dryRun, false)
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be removed")
	return cmd
}

func daemonCmd() *cobra.Command {
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Continuously reconcile (run under systemd)",
		Long: `Reconcile on start, every --interval, on interface/address changes and on SIGHUP.

Stopping the daemon leaves the current host state in place; use
'tailbridge down' to remove it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireRoot(); err != nil {
				return err
			}
			return runDaemon(cmd.Context(), interval)
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 30*time.Second, "periodic reconcile interval")
	return cmd
}

func runDaemon(ctx context.Context, interval time.Duration) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	// Interface and address changes trigger a reconcile after a short
	// debounce so that bursts of events cause a single pass.
	nlEvents := make(chan struct{}, 1)
	done := make(chan struct{})
	defer close(done)
	links := make(chan netlink.LinkUpdate, 16)
	addrs := make(chan netlink.AddrUpdate, 16)
	if err := netlink.LinkSubscribe(links, done); err != nil {
		log.Warn("netlink link subscription failed; relying on periodic reconcile", "err", err)
	}
	if err := netlink.AddrSubscribe(addrs, done); err != nil {
		log.Warn("netlink address subscription failed; relying on periodic reconcile", "err", err)
	}
	go func() {
		for {
			select {
			case <-links:
			case <-addrs:
			case <-done:
				return
			}
			select {
			case nlEvents <- struct{}{}:
			default:
			}
		}
	}()

	eng := newEngine()
	last := map[string]string{}
	pass := func(reason string) {
		if ctx.Err() != nil {
			return
		}
		err := withLock(func(cfg *config.Config) error {
			res, err := eng.Reconcile(ctx, cfg, engine.Options{})
			if res == nil {
				return err
			}
			if res.Applied {
				log.Info("reconciled", "trigger", reason, "changes", strings.Join(res.Plan.Steps(), "; "))
			}
			seen := map[string]string{}
			for _, m := range res.Mappings {
				summary := m.State + ": " + strings.Join(slices.Concat(m.Errors, m.Warnings), "; ")
				seen[m.Name] = summary
				if last[m.Name] != summary {
					lvl := slog.LevelInfo
					if m.State == engine.StateError {
						lvl = slog.LevelError
					} else if len(m.Warnings) > 0 {
						lvl = slog.LevelWarn
					}
					log.Log(ctx, lvl, "mapping status", "name", m.Name, "kind", m.Kind, "state", m.State,
						"errors", strings.Join(m.Errors, "; "), "warnings", strings.Join(m.Warnings, "; "))
				}
			}
			for _, w := range res.Warnings {
				if last["\x00"+w] == "" {
					log.Warn(w)
				}
				seen["\x00"+w] = "1"
			}
			last = seen
			return err
		})
		if err != nil && ctx.Err() == nil {
			log.Error("reconcile failed", "trigger", reason, "err", err)
		}
	}

	log.Info("tailbridge daemon starting", "version", version, "config", g.configPath, "interval", interval)
	pass("start")
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("tailbridge daemon stopping; host state left in place")
			return nil
		case <-tick.C:
			pass("interval")
		case <-hup:
			pass("SIGHUP")
		case <-nlEvents:
			time.Sleep(time.Second)
			select {
			case <-nlEvents:
			default:
			}
			pass("netlink")
		}
	}
}
