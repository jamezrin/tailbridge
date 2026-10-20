package netif

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"time"

	"golang.org/x/sys/unix"
)

// ProbeARP sends RFC 5227 ARP probes for addr on iface and returns the MAC
// address of a host that answers, or "" if nobody claims the address within
// timeout.
func ProbeARP(iface string, addr netip.Addr, timeout time.Duration) (string, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return "", fmt.Errorf("interface %s: %w", iface, err)
	}
	if len(ifi.HardwareAddr) != 6 {
		return "", fmt.Errorf("interface %s is not an Ethernet-like interface", iface)
	}
	proto := htons(unix.ETH_P_ARP)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, int(proto))
	if err != nil {
		return "", fmt.Errorf("arp socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: proto, Ifindex: ifi.Index}); err != nil {
		return "", fmt.Errorf("arp bind: %w", err)
	}
	tv := unix.NsecToTimeval((100 * time.Millisecond).Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return "", err
	}

	target := addr.As4()
	// ARP probe: sender IP 0.0.0.0, target IP addr.
	pkt := make([]byte, 28)
	binary.BigEndian.PutUint16(pkt[0:], 1)      // Ethernet
	binary.BigEndian.PutUint16(pkt[2:], 0x0800) // IPv4
	pkt[4], pkt[5] = 6, 4
	binary.BigEndian.PutUint16(pkt[6:], 1) // request
	copy(pkt[8:14], ifi.HardwareAddr)
	copy(pkt[24:28], target[:])
	bcast := &unix.SockaddrLinklayer{Protocol: proto, Ifindex: ifi.Index, Halen: 6}
	copy(bcast.Addr[:], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	deadline := time.Now().Add(timeout)
	nextSend := time.Now()
	buf := make([]byte, 128)
	for time.Now().Before(deadline) {
		if time.Now().After(nextSend) {
			if err := unix.Sendto(fd, pkt, 0, bcast); err != nil {
				return "", fmt.Errorf("arp send: %w", err)
			}
			nextSend = time.Now().Add(timeout / 3)
		}
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			continue // timeout tick
		}
		if n < 28 || binary.BigEndian.Uint16(buf[2:]) != 0x0800 {
			continue
		}
		sha := net.HardwareAddr(buf[8:14])
		spa := buf[14:18]
		tpa := buf[24:28]
		if sha.String() == ifi.HardwareAddr.String() {
			continue
		}
		op := binary.BigEndian.Uint16(buf[6:])
		// A reply from the address, or another probe for the same address.
		if (op == 2 && [4]byte(spa) == target) || (op == 1 && [4]byte(spa) == [4]byte{} && [4]byte(tpa) == target) {
			return sha.String(), nil
		}
	}
	return "", nil
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }
