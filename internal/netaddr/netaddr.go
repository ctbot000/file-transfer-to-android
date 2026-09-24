// Package netaddr finds the addresses a phone on the same network can use to
// reach this computer.
package netaddr

import (
	"net"
	"net/netip"
	"sort"
	"strings"
)

// Candidate is one IPv4 address of this computer and the interface it
// belongs to.
type Candidate struct {
	IP        netip.Addr
	Interface string
}

// Candidates lists this computer's IPv4 addresses, the one a phone on the
// same Wi-Fi is most likely to reach first. Loopback and link-local
// addresses are left out.
func Candidates() []Candidate {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var addrs []ifaceAddr
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		list, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range list {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipnet.IP)
			if !ok {
				continue
			}
			addrs = append(addrs, ifaceAddr{
				name:         ifi.Name,
				ip:           ip.Unmap(),
				pointToPoint: ifi.Flags&net.FlagPointToPoint != 0,
			})
		}
	}
	return rank(addrs, defaultRouteIP())
}

type ifaceAddr struct {
	name         string
	ip           netip.Addr
	pointToPoint bool
}

// cgnat is the shared address space that Tailscale and some carriers use.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// rank orders addresses by how likely a phone on the local network can
// reach them: the address of the default route first, then private LAN
// addresses, with VPN, container and VM interfaces last. Ties keep the
// system's interface order.
func rank(addrs []ifaceAddr, preferred netip.Addr) []Candidate {
	type scored struct {
		Candidate
		score int
	}
	var list []scored
	seen := make(map[netip.Addr]bool)
	for _, a := range addrs {
		ip := a.ip
		if !ip.Is4() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() || seen[ip] {
			continue
		}
		seen[ip] = true
		score := 0
		if ip == preferred {
			score += 100
		}
		switch {
		case ip.IsPrivate():
			score += 50
		case cgnat.Contains(ip):
			score += 10
		}
		if a.pointToPoint || isVirtual(a.name) {
			score -= 60
		}
		list = append(list, scored{Candidate{IP: ip, Interface: a.name}, score})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].score > list[j].score })
	out := make([]Candidate, len(list))
	for i, s := range list {
		out[i] = s.Candidate
	}
	return out
}

// virtualPrefixes are interface name prefixes of tunnels, bridges and
// container or VM networks on macOS and Linux.
var virtualPrefixes = []string{
	"anpi", "awdl", "br-", "bridge", "docker", "gif", "llw", "stf", "tailscale",
	"tap", "tun", "utun", "vboxnet", "veth", "virbr", "vmnet", "wg", "zt",
}

// virtualWords appear in Windows' descriptive interface names.
var virtualWords = []string{"virtual", "vethernet", "vmware", "hyper-v", "wsl", "vpn", "loopback"}

func isVirtual(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	for _, w := range virtualWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// defaultRouteIP returns the local address of the default route. Connecting
// a UDP socket sends nothing: it only asks the kernel which address it would
// send from. The destination is TEST-NET-1, which is never contacted.
func defaultRouteIP() netip.Addr {
	conn, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return netip.Addr{}
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}
	}
	ip, _ := netip.AddrFromSlice(addr.IP)
	return ip.Unmap()
}
