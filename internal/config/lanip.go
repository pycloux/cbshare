package config

import "net"

// DetectLANIP returns the IPv4 address of the interface used for outbound
// traffic, falling back to the first private, non-loopback IPv4 address and
// finally to 127.0.0.1. No packet is sent: dialing UDP only selects a route.
func DetectLANIP() string {
	if conn, err := net.Dial("udp4", "192.0.2.1:9"); err == nil {
		addr := conn.LocalAddr().(*net.UDPAddr)
		conn.Close()
		if ip := addr.IP.To4(); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			return ip.String()
		}
	}
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				ipnet, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				if ip := ipnet.IP.To4(); ip != nil && ip.IsPrivate() {
					return ip.String()
				}
			}
		}
	}
	return "127.0.0.1"
}
