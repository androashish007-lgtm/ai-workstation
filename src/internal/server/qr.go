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

func firstLANIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		v4 := ipNet.IP.To4()
		if v4 != nil {
			return v4.String()
		}
	}
	return ""
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
