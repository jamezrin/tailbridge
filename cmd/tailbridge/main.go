// Command tailbridge exposes individual devices across a LAN and a tailnet
// by managing IPv4 aliases, NAT rules and per-device advertised routes
// alongside an existing tailscaled.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/jamezrin/tailbridge/internal/config"
	"github.com/jamezrin/tailbridge/internal/engine"
	"github.com/jamezrin/tailbridge/internal/netif"
	"github.com/jamezrin/tailbridge/internal/nft"
	"github.com/jamezrin/tailbridge/internal/state"
	"github.com/jamezrin/tailbridge/internal/sysctl"
	"github.com/jamezrin/tailbridge/internal/tsapi"
)

var version = "dev"

type globals struct {
	configPath string
	stateDir   string
	socket     string
	nftPath    string
	jsonOut    bool
}

var g globals

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	root := &cobra.Command{
		Use:           "tailbridge",
		Short:         "Expose individual devices across a LAN and a tailnet",
		Long:          "Tailbridge maps individual devices between a LAN and a tailnet using IPv4 aliases,\nnftables NAT and per-device advertised /32 routes on top of an existing tailscaled.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&g.configPath, "config", envOr("TAILBRIDGE_CONFIG", "/etc/tailbridge/config.json"), "mapping configuration file")
	pf.StringVar(&g.stateDir, "state-dir", envOr("TAILBRIDGE_STATE_DIR", "/var/lib/tailbridge"), "directory for ownership state and lock")
	pf.StringVar(&g.socket, "socket", envOr("TAILBRIDGE_SOCKET", "/var/run/tailscale/tailscaled.sock"), "tailscaled Local API socket")
	pf.StringVar(&g.nftPath, "nft", envOr("TAILBRIDGE_NFT", "nft"), "nft binary")
	pf.BoolVar(&g.jsonOut, "json", false, "print machine-readable JSON")

	root.AddCommand(
		lanCmd(), tailnetCmd(), removeCmd(), listCmd(), statusCmd(),
		applyCmd(), downCmd(), daemonCmd(), diagnoseCmd(),
	)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "tailbridge:", err)
		os.Exit(1)
	}
}

func newEngine() *engine.Engine {
	return &engine.Engine{
		TS:    tsapi.New(g.socket),
		Net:   netif.Netlink{},
		NFT:   nft.Exec{Path: g.nftPath},
		Fwd:   sysctl.Proc{},
		State: state.Dir(g.stateDir),
	}
}

func requireRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("this command changes host networking and must run as root")
	}
	return nil
}

// withLock runs fn while holding the state lock, with the current config.
func withLock(fn func(cfg *config.Config) error) error {
	unlock, err := state.Dir(g.stateDir).Lock(30 * time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := config.Load(g.configPath)
	if err != nil {
		return err
	}
	return fn(cfg)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func lockState() (func(), error) {
	return state.Dir(g.stateDir).Lock(30 * time.Second)
}
