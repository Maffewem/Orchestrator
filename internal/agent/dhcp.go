package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// A small DHCPv4 client (RFC 2131) so a worker's macvlan address can be leased under the
// worker's own MAC address, which lets a router hand out and reserve a fixed address for it.
// It speaks to the LAN through UDP sockets bound to the parent interface, so it needs no
// raw sockets and no external DHCP program.

const (
	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpAck      = 5
	dhcpNak      = 6

	dhcpMagic = 0x63825363
)

// dhcpLease is an address the DHCP server handed out.
type dhcpLease struct {
	IP     net.IP
	Mask   net.IPMask
	Router net.IP
	Server net.IP
	Lease  time.Duration
}

// Subnet returns the lease's network, for example 192.168.0.0/24.
func (l *dhcpLease) Subnet() *net.IPNet {
	return &net.IPNet{IP: l.IP.Mask(l.Mask), Mask: l.Mask}
}

type dhcpClient struct {
	iface  string           // interface to bind sockets to; empty binds to none (tests)
	mac    net.HardwareAddr // the worker's address, used as chaddr and client identifier
	server string           // where requests go; default 255.255.255.255:67
	listen string           // where answers arrive; default :68
	// timeout is how long to wait for each answer, and tries how many times to ask.
	timeout time.Duration
	tries   int
	// control, set per platform, binds the socket to iface and allows broadcast and port sharing.
	control func(iface string) func(network, address string, c syscall.RawConn) error
}

func newDHCPClient(iface string, mac net.HardwareAddr) *dhcpClient {
	return &dhcpClient{iface: iface, mac: mac, server: "255.255.255.255:67", listen: ":68", timeout: 3 * time.Second, tries: 3, control: dhcpControl}
}

func (c *dhcpClient) build(kind byte, xid uint32, ciaddr, want, server net.IP) []byte {
	b := make([]byte, 240, 300)
	b[0], b[1], b[2] = 1, 1, 6 // BOOTREQUEST, ethernet, hardware address length
	binary.BigEndian.PutUint32(b[4:], xid)
	binary.BigEndian.PutUint16(b[10:], 0x8000) // ask for broadcast answers: the address isn't ours yet
	if ip := ciaddr.To4(); ip != nil {
		copy(b[12:16], ip)
	}
	copy(b[28:], c.mac)
	binary.BigEndian.PutUint32(b[236:], dhcpMagic)
	opt := func(code byte, v ...byte) { b = append(b, append([]byte{code, byte(len(v))}, v...)...) }
	opt(53, kind)
	opt(61, append([]byte{1}, c.mac...)...)
	opt(55, 1, 3, 6, 51, 54) // mask, router, DNS, lease time, server id
	if kind == dhcpRequest {
		if ip := want.To4(); ip != nil {
			opt(50, ip...)
		}
		if ip := server.To4(); ip != nil {
			opt(54, ip...)
		}
	}
	return append(b, 255)
}

type dhcpReply struct {
	kind   byte
	xid    uint32
	yiaddr net.IP
	lease  dhcpLease
}

func (c *dhcpClient) parse(b []byte, xid uint32) (*dhcpReply, bool) {
	if len(b) < 241 || b[0] != 2 || binary.BigEndian.Uint32(b[4:]) != xid || binary.BigEndian.Uint32(b[236:]) != dhcpMagic {
		return nil, false
	}
	if !bytes.Equal(b[28:28+len(c.mac)], c.mac) {
		return nil, false
	}
	r := &dhcpReply{xid: xid, yiaddr: net.IP(append([]byte(nil), b[16:20]...))}
	r.lease.IP = r.yiaddr
	for o := b[240:]; len(o) > 0; {
		code := o[0]
		if code == 255 {
			break
		}
		if code == 0 {
			o = o[1:]
			continue
		}
		if len(o) < 2 || len(o) < 2+int(o[1]) {
			return nil, false
		}
		v := o[2 : 2+int(o[1])]
		o = o[2+int(o[1]):]
		switch {
		case code == 53 && len(v) == 1:
			r.kind = v[0]
		case code == 1 && len(v) == 4:
			r.lease.Mask = net.IPMask(append([]byte(nil), v...))
		case code == 3 && len(v) >= 4:
			r.lease.Router = net.IP(append([]byte(nil), v[:4]...))
		case code == 54 && len(v) == 4:
			r.lease.Server = net.IP(append([]byte(nil), v...))
		case code == 51 && len(v) == 4:
			r.lease.Lease = time.Duration(binary.BigEndian.Uint32(v)) * time.Second
		}
	}
	return r, r.kind != 0
}

// exchange sends one message until an answer of one of the wanted kinds arrives.
func (c *dhcpClient) exchange(ctx context.Context, conn net.PacketConn, dst net.Addr, msg []byte, xid uint32, kinds ...byte) (*dhcpReply, error) {
	buf := make([]byte, 1500)
	for try := 0; try < c.tries; try++ {
		if _, err := conn.WriteTo(msg, dst); err != nil {
			return nil, fmt.Errorf("send: %w", err)
		}
		deadline := time.Now().Add(c.timeout)
		for time.Now().Before(deadline) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			_ = conn.SetReadDeadline(deadline)
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				break // timed out: ask again
			}
			if r, ok := c.parse(buf[:n], xid); ok {
				if r.kind == dhcpNak {
					return r, errors.New("the DHCP server refused the address (NAK)")
				}
				for _, k := range kinds {
					if r.kind == k {
						return r, nil
					}
				}
			}
		}
	}
	return nil, errors.New("no answer from a DHCP server on this network")
}

func (c *dhcpClient) open() (net.PacketConn, net.Addr, error) {
	dst, err := net.ResolveUDPAddr("udp4", c.server)
	if err != nil {
		return nil, nil, err
	}
	lc := net.ListenConfig{}
	if c.control != nil {
		lc.Control = c.control(c.iface)
	}
	conn, err := lc.ListenPacket(context.Background(), "udp4", c.listen)
	if err != nil {
		return nil, nil, fmt.Errorf("listen for DHCP answers: %w", err)
	}
	return conn, dst, nil
}

func newXID() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

// Offer asks what address the server would give this MAC without taking it.
func (c *dhcpClient) Offer(ctx context.Context) (*dhcpLease, error) {
	conn, dst, err := c.open()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	xid := newXID()
	r, err := c.exchange(ctx, conn, dst, c.build(dhcpDiscover, xid, nil, nil, nil), xid, dhcpOffer)
	if err != nil {
		return nil, err
	}
	l := r.lease
	return &l, nil
}

// Acquire leases an address: discover, request the offer, wait for the acknowledgement.
func (c *dhcpClient) Acquire(ctx context.Context) (*dhcpLease, error) {
	conn, dst, err := c.open()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	xid := newXID()
	offer, err := c.exchange(ctx, conn, dst, c.build(dhcpDiscover, xid, nil, nil, nil), xid, dhcpOffer)
	if err != nil {
		return nil, err
	}
	ack, err := c.exchange(ctx, conn, dst, c.build(dhcpRequest, xid, nil, offer.yiaddr, offer.lease.Server), xid, dhcpAck)
	if err != nil {
		return nil, err
	}
	l := ack.lease
	if l.Mask == nil {
		l.Mask = offer.lease.Mask
	}
	if l.Router == nil {
		l.Router = offer.lease.Router
	}
	if l.Server == nil {
		l.Server = offer.lease.Server
	}
	if l.Lease == 0 {
		l.Lease = offer.lease.Lease
	}
	return &l, nil
}

// Renew extends a lease the worker already holds, answering a broadcast REQUEST that names its address.
func (c *dhcpClient) Renew(ctx context.Context, cur *dhcpLease) (*dhcpLease, error) {
	conn, dst, err := c.open()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	xid := newXID()
	ack, err := c.exchange(ctx, conn, dst, c.build(dhcpRequest, xid, cur.IP, nil, nil), xid, dhcpAck)
	if err != nil {
		return nil, err
	}
	l := ack.lease
	if !l.IP.Equal(cur.IP) {
		return nil, fmt.Errorf("the DHCP server answered with %s instead of %s", l.IP, cur.IP)
	}
	l.Mask, l.Router = cur.Mask, cur.Router
	if l.Lease == 0 {
		l.Lease = cur.Lease
	}
	return &l, nil
}
