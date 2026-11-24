//go:build linux

package agent

import (
	"net"
	"testing"
	"time"
)

// A packet socket on loopback receives a DHCP-shaped datagram (needs CAP_NET_RAW, skipped otherwise).
func TestPacketRecvOnLoopback(t *testing.T) {
	r, err := newRawRecv("lo")
	if err != nil {
		t.Skipf("no packet socket: %v", err)
	}
	defer r.Close()
	srv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 67})
	if err != nil {
		t.Skipf("port 67: %v", err)
	}
	defer srv.Close()
	_ = r.SetReadDeadline(time.Now().Add(2 * time.Second))
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = srv.WriteToUDP([]byte("offer"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 68})
	}()
	buf := make([]byte, 64)
	n, _, err := r.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "offer" || r.Seen() != 1 {
		t.Fatalf("n=%d err=%v seen=%d", n, err, r.Seen())
	}
}
