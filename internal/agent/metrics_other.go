//go:build !linux

package agent

import "github.com/Maffewem/Orchestrator/internal/api"

func meminfo() map[string]uint64 { return map[string]uint64{} }

// metrics is only implemented for Linux, the agent's target platform.
func (a *Agent) metrics() api.Metrics { return api.Metrics{} }
