package server

import (
	"net"
	"net/http"

	qrcode "github.com/skip2/go-qrcode"
)

// LANURL returns the best-guess http://<ip>:<port> other devices on the same
// network should use — the first non-loopback IPv4 address found.
func LANURL(port int) string {
	ip := firstLANIPv4()
	if ip == "" {
		return ""
	}
	return "http://" + ip + ":" + itoa(port)
}

// firstLANIPv4 picks the IPv4 address other devices on the same network
// should actually use. A plain "first non-loopback address" is wrong on
// most real machines: Windows especially tends to enumerate several virtual
// adapters (VPN clients, Hyper-V/VirtualBox host-only networks, Bluetooth
// PAN) ahead of the real Wi-Fi/Ethernet one, and a disconnected or
// not-yet-DHCP'd adapter reports a 169.254.x.x link-local address that
// looks superficially fine but nothing else on the network can reach. So:
// only consider interfaces that are actually up, skip link-local addresses
// outright, and prefer a private (RFC 1918) address — the shape a normal
// home/office LAN IP actually has — over any other candidate.
func firstLANIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	var fallback string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			v4 := ipNet.IP.To4()
			if v4 == nil || v4.IsLoopback() || v4.IsLinkLocalUnicast() {
				continue
			}
			if isPrivateIPv4(v4) {
				return v4.String() // best match: a real LAN address
			}
			if fallback == "" {
				fallback = v4.String()
			}
		}
	}
	return fallback
}

func isPrivateIPv4(ip net.IP) bool {
	return ip[0] == 10 ||
		(ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31) ||
		(ip[0] == 192 && ip[1] == 168)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// handleQR renders a QR code PNG for the URL passed in the `url` query
// param (the browser knows its own host:port; we just render whatever it
// asks for) so phones/tablets on the same network can scan in.
func (a *App) handleQR(w http.ResponseWriter, r *http.Request) {
	full := r.URL.Query().Get("url")
	if full == "" {
		http.Error(w, "missing url query param", http.StatusBadRequest)
		return
	}
	png, err := qrcode.Encode(full, qrcode.Medium, 256)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(png)
}
