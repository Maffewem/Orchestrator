//go:build !linux

package agent

import (
	"context"
	"errors"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func (a *Agent) imageAction(ctx context.Context, c api.Command) (string, error) {
	return "", errors.New("image rebuilds are only available on Linux devices")
}
