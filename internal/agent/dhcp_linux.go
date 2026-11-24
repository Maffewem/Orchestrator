//go:build linux

package agent

import (
	"net"
	"os"
	"syscall"
	"time"
)

// dhcpControl allows broadcasts and a shared port 68 (a DHCP client may already run on the
// device) and, when iface is set, keeps the socket on that interface.
func dhcpControl(iface string) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			h := int(fd)
			if serr = syscall.SetsockoptInt(h, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); serr != nil {
				return
			}
			if serr = syscall.SetsockoptInt(h, syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1); serr != nil {
				return
			}
			if iface != "" {
				serr = syscall.SetsockoptString(h, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, iface)
			}
		})
		if err != nil {
			return err
		}
		return serr
	}
}

// packetRecv reads IPv4 frames from one interface through an AF_PACKET socket.
type packetRecv struct {
	fd       int
	seen     int
	deadline time.Time
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

func newRawRecv(iface string) (rawReader, error) {
	ifc, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, int(htons(syscall.ETH_P_IP)))
	if err != nil {
		return nil, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: htons(syscall.ETH_P_IP), Ifindex: ifc.Index}); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return &packetRecv{fd: fd}, nil
}

func (r *packetRecv) Seen() int    { return r.seen }
func (r *packetRecv) Close() error { return syscall.Close(r.fd) }

func (r *packetRecv) SetReadDeadline(t time.Time) error {
	r.deadline = t
	d := time.Until(t)
	if d < time.Millisecond {
		d = time.Millisecond
	}
	tv := syscall.NsecToTimeval(d.Nanoseconds())
	return syscall.SetsockoptTimeval(r.fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
}

func (r *packetRecv) ReadFrom(b []byte) (int, net.Addr, error) {
	buf := make([]byte, 2048)
	for {
		if !r.deadline.IsZero() && time.Now().After(r.deadline) {
			return 0, nil, os.ErrDeadlineExceeded
		}
		n, _, err := syscall.Recvfrom(r.fd, buf, 0)
		switch {
		case err == syscall.EINTR:
			continue
		case err == syscall.EAGAIN:
			return 0, nil, os.ErrDeadlineExceeded
		case err != nil:
			return 0, nil, err
		}
		if p := dhcpPayload(buf[:n]); p != nil {
			r.seen++
			return copy(b, p), &net.UDPAddr{}, nil
		}
	}
}
