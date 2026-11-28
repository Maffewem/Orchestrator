package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Agent fencing, server side. An agent that has seen a higher server epoch refuses servers
// reporting a lower one (see internal/agent/fence.go) and tells the server it does talk to
// which stale servers it ignored. They are shown as a warning on the device, because a stale
// server is usually an old active server that came back after a failover and has not yet
// stepped down.

// staleKeep is how long a report counts after the agent last mentioned it.
const staleKeep = 20 * time.Minute

type staleServers struct {
	mu sync.Mutex
	m  map[string]staleReport // device id
}

type staleReport struct {
	Servers []api.StaleServer
	At      time.Time
}

// noteStale records what a device's heartbeat said about stale servers, and logs an event the
// first time a given stale server is reported for the device.
func (s *Server) noteStale(ctx context.Context, deviceName, deviceID string, stale []api.StaleServer) {
	if len(stale) > 10 { // an agent can't flood the activity log with invented servers
		stale = stale[:10]
	}
	s.stale.mu.Lock()
	if s.stale.m == nil {
		s.stale.m = map[string]staleReport{}
	}
	prev := s.stale.m[deviceID]
	if len(stale) == 0 {
		delete(s.stale.m, deviceID)
		s.stale.mu.Unlock()
		return
	}
	s.stale.m[deviceID] = staleReport{Servers: stale, At: time.Now()}
	s.stale.mu.Unlock()
	known := map[string]bool{}
	for _, p := range prev.Servers {
		known[p.URL] = true
	}
	for _, st := range stale {
		if !known[st.URL] {
			s.event(ctx, deviceID, "warning", "device.stale_server", fmt.Sprintf(
				"%s ignored %s: it reports an older server epoch (%d) than one the device has already seen", deviceName, st.URL, st.Epoch))
		}
	}
}

// staleFor returns the stale servers a device reported recently.
func (s *Server) staleFor(deviceID string) []api.StaleServer {
	s.stale.mu.Lock()
	defer s.stale.mu.Unlock()
	r, ok := s.stale.m[deviceID]
	if !ok || time.Since(r.At) > staleKeep {
		return nil
	}
	return r.Servers
}

// staleIssue is the dashboard warning for a device that is ignoring a stale server.
func staleIssue(stale []api.StaleServer) string {
	urls := make([]string, 0, len(stale))
	for _, st := range stale {
		urls = append(urls, st.URL)
	}
	return fmt.Sprintf("This device is ignoring %s: it reports an older server epoch, so it is an old server that was replaced. Switch it off or let it rejoin as a standby.", strings.Join(urls, ", "))
}
