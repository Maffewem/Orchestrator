package agent

import (
	"errors"
	"os"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Signed work (see api/sign.go). The agent trusts the server key it was given at registration
// and never replaces it by itself: over plain HTTP, a replaced key would let a network attacker
// sign its own commands. ORCH_REPIN_SERVER_KEY=1 lets the next heartbeat set a new one, for a
// server that was rebuilt without its identity key.

var errUnsigned = errors.New("refusing unsigned work: this server has signed everything before, so the message did not come from it")

// acceptSigned decides whether a message is acceptable. sig is its signature, valid reports
// whether that signature checks out against the pinned key.
func (a *Agent) acceptSigned(sig string, valid func(key string) bool) error {
	a.epochMu.Lock()
	key, seen := a.state.ServerKey, a.state.SignedSeen
	a.epochMu.Unlock()
	if sig == "" {
		if seen {
			return errUnsigned
		}
		return nil // an older server that does not sign yet
	}
	if key == "" || !valid(key) {
		return errors.New("refusing work whose signature does not match this agent's server")
	}
	if !seen {
		a.epochMu.Lock()
		a.state.SignedSeen = true
		a.epochMu.Unlock()
		a.saveState()
	}
	return nil
}

func (a *Agent) acceptCommand(c api.Command) error {
	if err := a.acceptSigned(c.Sig, func(key string) bool { return api.VerifyCommand(key, c) }); err != nil {
		return err
	}
	if c.Sig != "" && c.DeviceID != a.state.DeviceID {
		return errors.New("refusing a command signed for another device")
	}
	return nil
}

func (a *Agent) acceptStream(r api.StreamRequest) error {
	return a.acceptSigned(r.Sig, func(key string) bool {
		return api.VerifyText(key, api.StreamSigningText(a.state.DeviceID, r), r.Sig)
	})
}

func (a *Agent) acceptUpdate(arch string, i api.AgentUpdateInfo) error {
	return a.acceptSigned(i.Sig, func(key string) bool { return api.VerifyText(key, api.UpdateSigningText(arch, i), i.Sig) })
}

func repinAllowed() bool { return os.Getenv("ORCH_REPIN_SERVER_KEY") == "1" }
