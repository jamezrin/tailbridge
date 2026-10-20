// Command fakets is a minimal stand-in for tailscaled's Local API, used by
// the integration tests. It serves /localapi/v0/status and
// /localapi/v0/prefs from a JSON file that tests may edit at any time, and
// fails loudly on any other request.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
)

type node struct {
	// ID is the stable node ID; defaults to "id-" + hostName.
	ID       string       `json:"id,omitempty"`
	HostName string       `json:"hostName"`
	DNSName  string       `json:"dnsName"`
	IPs      []netip.Addr `json:"ips"`
	Online   bool         `json:"online"`
	// PrimaryRoutes are routes this peer serves (to test overlap checks).
	PrimaryRoutes []netip.Prefix `json:"primaryRoutes,omitempty"`
}

type fakeState struct {
	BackendState string `json:"backendState"`
	Self         node   `json:"self"`
	Peers        []node `json:"peers"`
	// Approve controls whether advertised routes become primary routes.
	Approve         bool           `json:"approve"`
	AdvertiseRoutes []netip.Prefix `json:"advertiseRoutes"`
	// Writes counts EditPrefs calls, for assertions.
	Writes int `json:"writes"`
}

type server struct {
	path string
	mu   sync.Mutex
	keys map[string]key.NodePublic
}

func (s *server) load() (*fakeState, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	var st fakeState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *server) save(st *fakeState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *server) key(name string) key.NodePublic {
	if k, ok := s.keys[name]; ok {
		return k
	}
	k := key.NewNode().Public()
	s.keys[name] = k
	return k
}

func peerStatus(n node, routes []netip.Prefix) *ipnstate.PeerStatus {
	id := n.ID
	if id == "" {
		id = "id-" + n.HostName
	}
	ps := &ipnstate.PeerStatus{
		ID:           tailcfg.StableNodeID(id),
		HostName:     n.HostName,
		DNSName:      n.DNSName,
		TailscaleIPs: n.IPs,
		Online:       n.Online,
		OS:           "linux",
	}
	if len(routes) > 0 {
		v := views.SliceOf(routes)
		ps.PrimaryRoutes = &v
	}
	return ps
}

func (s *server) prefs(st *fakeState) *ipn.Prefs {
	p := ipn.NewPrefs()
	p.WantRunning = true
	p.AdvertiseRoutes = st.AdvertiseRoutes
	return p
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Host != "local-tailscaled.sock" {
		http.Error(w, "bad host "+r.Host, http.StatusForbidden)
		return
	}
	st, err := s.load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/localapi/v0/status":
		var primary []netip.Prefix
		if st.Approve {
			for _, rt := range st.AdvertiseRoutes {
				if rt.Bits() > 0 {
					primary = append(primary, rt)
				}
			}
		}
		out := &ipnstate.Status{
			Version:      "fake",
			BackendState: st.BackendState,
			TailscaleIPs: st.Self.IPs,
			Self:         peerStatus(st.Self, primary),
			Peer:         map[key.NodePublic]*ipnstate.PeerStatus{},
		}
		out.Self.Online = true
		for _, p := range st.Peers {
			out.Peer[s.key(p.DNSName)] = peerStatus(p, p.PrimaryRoutes)
		}
		writeJSON(w, out)
	case r.Method == http.MethodGet && r.URL.Path == "/localapi/v0/prefs":
		writeJSON(w, s.prefs(st))
	case r.Method == http.MethodPatch && r.URL.Path == "/localapi/v0/prefs":
		var mp ipn.MaskedPrefs
		if err := json.NewDecoder(r.Body).Decode(&mp); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if other := otherSetFields(&mp); len(other) > 0 {
			log.Printf("fakets: unexpected prefs fields set: %v", other)
			http.Error(w, "fakets: unexpected prefs fields set: "+strings.Join(other, ","), http.StatusBadRequest)
			return
		}
		if mp.AdvertiseRoutesSet {
			st.AdvertiseRoutes = mp.AdvertiseRoutes
		}
		st.Writes++
		if err := s.save(st); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, s.prefs(st))
	default:
		log.Printf("fakets: unexpected request %s %s", r.Method, r.URL)
		http.Error(w, "fakets: unexpected request", http.StatusNotFound)
	}
}

// otherSetFields returns the names of MaskedPrefs *Set fields other than
// AdvertiseRoutesSet that are true: Tailbridge must never touch them.
func otherSetFields(mp *ipn.MaskedPrefs) []string {
	var out []string
	v := reflect.ValueOf(mp).Elem()
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if strings.HasSuffix(f.Name, "Set") && f.Type.Kind() == reflect.Bool && f.Name != "AdvertiseRoutesSet" && v.Field(i).Bool() {
			out = append(out, f.Name)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	socket := flag.String("socket", "", "unix socket to listen on")
	statePath := flag.String("state", "", "JSON state file")
	flag.Parse()
	if *socket == "" || *statePath == "" {
		fmt.Fprintln(os.Stderr, "usage: fakets -socket PATH -state FILE")
		os.Exit(2)
	}
	_ = os.MkdirAll(filepath.Dir(*socket), 0o755)
	_ = os.Remove(*socket)
	ln, err := net.Listen("unix", *socket)
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(http.Serve(ln, &server{path: *statePath, keys: map[string]key.NodePublic{}}))
}
