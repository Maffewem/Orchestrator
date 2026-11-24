package agent

import (
	"net"
	"strings"
	"testing"
)

func testLAN() *lan {
	_, sn, _ := net.ParseCIDR("192.168.0.0/24")
	return &lan{Parent: "eth0", Subnet: sn, Gateway: net.ParseIP("192.168.0.1"), Own: []net.IP{net.ParseIP("192.168.0.184")}}
}

func TestStaticProblems(t *testing.T) {
	cases := []struct {
		ip   string
		want string // substring of the first problem; empty means fine
	}{
		{"192.168.0.200", ""},
		{"192.168.1.5", "outside"},
		{"192.168.0.0", "network or broadcast"},
		{"192.168.0.255", "network or broadcast"},
		{"192.168.0.1", "gateway"},
		{"192.168.0.184", "device itself"},
	}
	for _, c := range cases {
		ps := staticProblems(net.ParseIP(c.ip), testLAN())
		switch {
		case c.want == "" && len(ps) > 0:
			t.Errorf("%s: unexpected %v", c.ip, ps)
		case c.want != "" && (len(ps) == 0 || !strings.Contains(ps[0], c.want)):
			t.Errorf("%s: want %q, got %v", c.ip, c.want, ps)
		}
	}
}

func TestParseMAC(t *testing.T) {
	if _, err := parseMAC("02:42:ac:11:00:09"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "nonsense", "01:00:5e:00:00:01", "02:42:ac:11:00"} {
		if _, err := parseMAC(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestSplitVLANAndShim(t *testing.T) {
	phys, id, ok := splitVLAN("eth0.1005")
	if !ok || phys != "eth0" || id != 1005 {
		t.Fatalf("got %q %d %v", phys, id, ok)
	}
	for _, n := range []string{"eth0", "eth0.", "eth0.0", "eth0.4095", "eth0.x", ".5"} {
		if _, _, ok := splitVLAN(n); ok {
			t.Errorf("%q is not a VLAN sub-interface", n)
		}
	}
	if shimFor("eth0") != "orch-shim" || shimFor("eth0.1005") != "orch-shimv1005" {
		t.Fatalf("shim names: %s %s", shimFor("eth0"), shimFor("eth0.1005"))
	}
	if len(shimFor("eth0.4094")) > 15 {
		t.Fatal("interface names are limited to 15 characters")
	}
}

func TestVLANLearnsSubnetFromLease(t *testing.T) {
	l := &lan{Parent: "eth0.1005", Phys: "eth0"}
	l.adopt(&dhcpLease{IP: net.ParseIP("192.168.5.77"), Mask: net.CIDRMask(24, 32), Router: net.ParseIP("192.168.5.1")})
	if l.Subnet.String() != "192.168.5.0/24" || l.Gateway.String() != "192.168.5.1" || l.physical() != "eth0" {
		t.Fatalf("%+v", l)
	}
	l = &lan{Parent: "eth0.1005", Phys: "eth0"}
	l.adopt(&dhcpLease{IP: net.ParseIP("192.168.5.77"), Mask: net.CIDRMask(24, 32), Router: net.ParseIP("10.0.0.1")})
	if l.Gateway != nil {
		t.Fatal("a router outside the subnet isn't a usable gateway")
	}
}
