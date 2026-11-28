package agent

import (
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func TestPowerActionValidates(t *testing.T) {
	a := &Agent{}
	for _, args := range []map[string]string{
		{"mode": "halt", "confirm": "halt"},
		{"mode": "reboot", "confirm": "poweroff"},
		{"mode": "reboot"},
	} {
		if _, err := a.powerAction(api.Command{Args: args}); err == nil {
			t.Errorf("%v should be refused", args)
		}
		if a.afterReport != nil {
			t.Errorf("%v queued work although refused", args)
		}
	}
}
