//go:build !linux

package agent

import (
	"context"
	"errors"

	"github.com/Maffewem/Orchestrator/internal/api"
)

type hardware struct{}

func (a *Agent) hardware() *hardware           { return nil }
func hardwareCaps() []string                   { return nil }
func (a *Agent) startHardware(context.Context) {}
func (a *Agent) hardwareCommand(context.Context, api.Command) (bool, string, error) {
	return false, "", nil
}
func (a *Agent) serveConsole(context.Context, api.StreamRequest) error {
	return errors.New("the console is only available on Linux devices")
}

// BladeLEDTest is only available on Linux.
func BladeLEDTest(interface{ Write([]byte) (int, error) }) error {
	return errors.New("the blade LED test only runs on Linux")
}
