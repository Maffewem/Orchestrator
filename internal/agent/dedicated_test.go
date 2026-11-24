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
