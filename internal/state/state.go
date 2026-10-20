// Package state records which host resources Tailbridge itself created, so
// that cleanup only ever touches Tailbridge-owned state.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jamezrin/tailbridge/internal/config"
)

// OwnedAddr is an IPv4 /32 that Tailbridge added to an interface.
type OwnedAddr struct {
	Interface string     `json:"interface"`
	Addr      netip.Addr `json:"addr"`
}

// State is persisted ownership and cache information.
type State struct {
	Version int `json:"version"`
	// Addresses Tailbridge added and must remove when no longer desired.
	Addresses []OwnedAddr `json:"addresses"`
	// Routes Tailbridge added to tailscaled's AdvertiseRoutes. Routes that
	// were already advertised by someone else are never recorded here.
	Routes []netip.Prefix `json:"routes"`
	// Peers pins the node each lan mapping resolved to and caches its last
	// Tailscale IPv4, keyed by mapping name.
	Peers map[string]CachedPeer `json:"peers"`
}

// CachedPeer is the node a lan mapping's peer spec resolved to.
type CachedPeer struct {
	// Spec is the peer spec the entry was resolved from; the entry is
	// ignored if the mapping's spec changes.
	Spec string     `json:"spec"`
	IP   netip.Addr `json:"ip"`
	// NodeID is the peer's stable node ID. A mapping never silently
	// retargets to a different node with the same name.
	NodeID string `json:"nodeID,omitempty"`
}

// HasAddr reports whether Tailbridge owns addr on iface.
func (s *State) HasAddr(iface string, addr netip.Addr) bool {
	return slices.Contains(s.Addresses, OwnedAddr{iface, addr})
}

// AddAddr records ownership of addr on iface.
func (s *State) AddAddr(iface string, addr netip.Addr) {
	if !s.HasAddr(iface, addr) {
		s.Addresses = append(s.Addresses, OwnedAddr{iface, addr})
	}
}

// DelAddr forgets ownership of addr on iface.
func (s *State) DelAddr(iface string, addr netip.Addr) {
	s.Addresses = slices.DeleteFunc(s.Addresses, func(o OwnedAddr) bool { return o == OwnedAddr{iface, addr} })
}

// Dir holds Tailbridge's state file and lock.
type Dir string

func (d Dir) statePath() string { return filepath.Join(string(d), "state.json") }

// Load reads the state, returning an empty state if none exists.
func (d Dir) Load() (*State, error) {
	s := &State{Version: 1, Peers: map[string]CachedPeer{}}
	b, err := os.ReadFile(d.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", d.statePath(), err)
	}
	if s.Peers == nil {
		s.Peers = map[string]CachedPeer{}
	}
	return s, nil
}

// Save atomically writes the state.
func (d Dir) Save(s *State) error {
	if s.Addresses == nil {
		s.Addresses = []OwnedAddr{}
	}
	if s.Routes == nil {
		s.Routes = []netip.Prefix{}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(d.statePath(), append(b, '\n'), 0o600)
}

// Lock takes an exclusive lock so that the CLI and daemon never reconcile
// at the same time. It waits up to timeout.
func (d Dir) Lock(timeout time.Duration) (unlock func(), err error) {
	if err := os.MkdirAll(string(d), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(string(d), "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) || time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w (is another tailbridge running?)", d, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
