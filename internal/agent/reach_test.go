package agent

import (
	"reflect"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func TestPublishedTCPPorts(t *testing.T) {
	got := publishedTCPPorts(api.DeploySpec{Ports: []string{"25664:3000", "53:53/udp", "9000:90/tcp", "80"}})
	if want := []string{"3000", "90"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
