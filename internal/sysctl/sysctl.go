// Package sysctl reads and enables IPv4 forwarding.
package sysctl

import (
	"os"
	"strings"
)

// Forwarding controls net.ipv4.ip_forward.
type Forwarding interface {
	Enabled() (bool, error)
	// Enable turns forwarding on. There is deliberately no Disable: other
	// software (tailscaled subnet routing, containers) may depend on it.
	Enable() error
}

// Proc uses /proc/sys.
type Proc struct{}

const ipForwardPath = "/proc/sys/net/ipv4/ip_forward"

func (Proc) Enabled() (bool, error) {
	b, err := os.ReadFile(ipForwardPath)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(b)) == "1", nil
}

func (Proc) Enable() error {
	return os.WriteFile(ipForwardPath, []byte("1\n"), 0o644)
}
