package netaddr

import (
	"net/netip"
	"testing"
)

func TestRank(t *testing.T) {
	ip := netip.MustParseAddr
	addrs := []ifaceAddr{
		{name: "utun4", ip: ip("100.101.102.103"), pointToPoint: true}, // Tailscale
		{name: "bridge100", ip: ip("192.168.64.1")},                    // VM network
		{name: "en7", ip: ip("10.0.0.20")},                             // Ethernet
		{name: "en0", ip: ip("192.168.1.23")},                          // Wi-Fi, default route
		{name: "en0", ip: ip("fe80::1")},                               // IPv6 link-local
		{name: "en0", ip: ip("169.254.10.10")},                         // IPv4 link-local
		{name: "lo0", ip: ip("127.0.0.1")},
		{name: "en0", ip: ip("192.168.1.23")}, // listed twice
	}
	got := rank(addrs, ip("192.168.1.23"))
	want := []string{"192.168.1.23", "10.0.0.20", "192.168.64.1", "100.101.102.103"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i].IP.String() != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestIsVirtual(t *testing.T) {
	for name, want := range map[string]bool{
		"en0": false, "wlan0": false, "eth0": false, "Wi-Fi": false, "Ethernet 2": false,
		"docker0": true, "br-1a2b": true, "vEthernet (WSL)": true, "utun3": true,
		"VirtualBox Host-Only Network": true, "tailscale0": true, "awdl0": true,
	} {
		if got := isVirtual(name); got != want {
			t.Errorf("isVirtual(%q) = %v, want %v", name, got, want)
		}
	}
}
