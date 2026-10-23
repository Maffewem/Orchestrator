package agent

import (
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func TestPortConflicts(t *testing.T) {
	cs := []api.Container{
		{Name: "/web", State: "running", Ports: []api.Port{{PublicPort: 8080, PrivatePort: 80, Type: "tcp"}}},
		{Name: "/other", State: "running", Ports: []api.Port{{PublicPort: 9000, PrivatePort: 90, Type: "tcp"}}},
		{Name: "/old", State: "exited", Ports: []api.Port{{PublicPort: 7000, PrivatePort: 70, Type: "tcp"}}},
	}
	free := func(string, int) bool { return false }
	busy := func(_ string, p int) bool { return p == 5432 }
	spec := func(ports ...string) api.DeploySpec { return api.DeploySpec{Service: "web", Ports: ports} }
	cases := []struct {
		ports []string
		inUse func(string, int) bool
		want  int
	}{
		{[]string{"8080:80"}, free, 0},         // own previous container
		{[]string{"9000:90"}, free, 1},         // another container
		{[]string{"7000:70"}, free, 0},         // stopped container
		{[]string{"5432:5432"}, busy, 1},       // host process
		{[]string{"8080:80", "3000"}, busy, 0}, // no host port
		{[]string{"9000:90/udp"}, free, 0},     // different protocol
	}
	for _, c := range cases {
		if got := portConflicts(spec(c.ports...), cs, c.inUse); len(got) != c.want {
			t.Errorf("%v: got %v", c.ports, got)
		}
	}
}
