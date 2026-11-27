//go:build !linux

package agent

import (
	"context"
	"errors"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func (a *Agent) diskAction(ctx context.Context, c api.Command) (string, error) {
	return "", errors.New("disk management is only available on Linux devices")
}
