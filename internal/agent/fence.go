package agent

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Epoch fencing. Every server stamps the epoch of its high-availability pair on each response
// (X-Orch-Epoch); a promoted standby carries a higher one than the server it replaced. The
// agent remembers the highest epoch it has ever seen and refuses to take commands, downloads
// or a shell from any server that reports a lower one, so a server that was cut off during a
// failover and comes back still believing it is in charge cannot act on the device. Servers
// that send no header (older builds, or an unpaired server) are never fenced, and an agent
// with no stored epoch accepts the first one it sees.

// staleKeep is how long a stale server stays on the warning after it was last seen.
const staleKeep = 15 * time.Minute

// errStaleServer means the server answered with an epoch lower than one the agent has seen.
type errStaleServer struct {
	URL          string
	Epoch, Known int
}

func (e *errStaleServer) Error() string {
	return fmt.Sprintf("%s is a stale server (epoch %d, but the agent has seen epoch %d); ignoring it", e.URL, e.Epoch, e.Known)
}

// fence checks the epoch header of resp. It raises the stored epoch when the server is newer
// and returns *errStaleServer when it is older. The caller closes resp.Body.
func (a *Agent) fence(resp *http.Response) error {
	h := strings.TrimSpace(resp.Header.Get("X-Orch-Epoch"))
	if h == "" {
		return nil
	}
	e, err := strconv.Atoi(h)
	if err != nil || e <= 0 {
		return nil
	}
	base := a.server()
	a.epochMu.Lock()
	known := a.state.Epoch
	switch {
	case e < known:
		first := a.stale[base].Seen.IsZero() || time.Since(a.stale[base].Seen) > staleKeep
		if a.stale == nil {
			a.stale = map[string]api.StaleServer{}
		}
		a.stale[base] = api.StaleServer{URL: base, Epoch: e, Seen: time.Now()}
		a.epochMu.Unlock()
		if first {
			a.log.Warn("ignoring a stale server: it reports an older epoch than one this agent has already seen",
				"server", base, "server_epoch", e, "known_epoch", known)
		}
		return &errStaleServer{URL: base, Epoch: e, Known: known}
	case e > known:
		a.state.Epoch = e
		delete(a.stale, base) // it caught up
		a.epochMu.Unlock()
		if known != 0 {
			a.log.Info("control plane epoch raised", "from", known, "to", e)
		}
		a.saveState()
		return nil
	}
	delete(a.stale, base)
	a.epochMu.Unlock()
	return nil
}

// epoch is the highest server epoch this agent has seen, 0 if none.
func (a *Agent) epoch() int {
	a.epochMu.Lock()
	defer a.epochMu.Unlock()
	return a.state.Epoch
}

// staleServers lists the stale servers seen recently, for the heartbeat to report.
func (a *Agent) staleServers() []api.StaleServer {
	a.epochMu.Lock()
	defer a.epochMu.Unlock()
	var out []api.StaleServer
	for u, s := range a.stale {
		if time.Since(s.Seen) > staleKeep {
			delete(a.stale, u)
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}
