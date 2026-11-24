package agent

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// fakeDHCP answers DISCOVER with an OFFER and REQUEST with an ACK (or a NAK) on a loopback port.
func fakeDHCP(t *testing.T, offer net.IP, nak bool) string {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			req := buf[:n]
			var kind byte
			for o := req[240:]; len(o) > 2 && o[0] != 255; o = o[2+int(o[1]):] {
				if o[0] == 53 {
					kind = o[2]
				}
			}
			reply := make([]byte, 240, 300)
			reply[0], reply[1], reply[2] = 2, 1, 6
			copy(reply[4:8], req[4:8])
			copy(reply[28:34], req[28:34])
			binary.BigEndian.PutUint32(reply[236:], dhcpMagic)
			typ := byte(dhcpOffer)
			if kind == dhcpRequest {
				typ = dhcpAck
				if nak {
					typ = dhcpNak
				}
			}
			copy(reply[16:20], offer.To4())
			reply = append(reply, 53, 1, typ, 1, 4, 255, 255, 255, 0, 3, 4, 192, 168, 0, 1, 54, 4, 192, 168, 0, 1, 51, 4, 0, 0, 0x0e, 0x10, 255)
			_, _ = conn.WriteTo(reply, from)
		}
	}()
	return conn.LocalAddr().String()
}

func testClient(server string) *dhcpClient {
	mac, _ := net.ParseMAC("02:42:ac:11:00:09")
	c := newDHCPClient("", mac)
	c.server, c.listen, c.control = server, "127.0.0.1:0", nil
	c.timeout, c.tries = 500*time.Millisecond, 2
	return c
}

func TestDHCPAcquire(t *testing.T) {
	c := testClient(fakeDHCP(t, net.ParseIP("192.168.0.77"), false))
	l, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if l.IP.String() != "192.168.0.77" || l.Router.String() != "192.168.0.1" || l.Lease != time.Hour {
		t.Fatalf("lease = %+v", l)
	}
	if got := l.Subnet().String(); got != "192.168.0.0/24" {
		t.Fatalf("subnet %s", got)
	}
	o, err := c.Offer(context.Background())
	if err != nil || o.IP.String() != "192.168.0.77" {
		t.Fatalf("offer %v %v", o, err)
	}
	r, err := c.Renew(context.Background(), l)
	if err != nil || r.IP.String() != "192.168.0.77" {
		t.Fatalf("renew %v %v", r, err)
	}
}

func TestDHCPNakAndSilence(t *testing.T) {
	if _, err := testClient(fakeDHCP(t, net.ParseIP("192.168.0.77"), true)).Acquire(context.Background()); err == nil {
		t.Fatal("a NAK should fail")
	}
	// Nothing listens here: the client must give up instead of hanging.
	c := testClient("127.0.0.1:9")
	if _, err := c.Offer(context.Background()); err == nil {
		t.Fatal("no server should fail")
	}
}

func TestDHCPIgnoresOtherClients(t *testing.T) {
	c := testClient("127.0.0.1:9")
	pkt := c.build(dhcpDiscover, 7, nil, nil, nil)
	pkt[0] = 2
	if _, ok := c.parse(pkt, 8); ok {
		t.Fatal("a reply for another transaction must be ignored")
	}
}
