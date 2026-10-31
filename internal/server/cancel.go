package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Maffewem/Orchestrator/internal/store"
)

// handleCancelDeployment gives up on a queued or running deployment, for example one stuck on
// a build or an agent that went away. The deployment is marked failed at once; an agent that is
// still working on it is told to stop at its next heartbeat. The old container keeps running
// unless the swap had already started, in which case the agent rolls it back.
func (s *Server) handleCancelDeployment(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDeployment(w, r)
	if !ok {
		return
	}
	if d.Status != store.DeploymentQueued && d.Status != store.DeploymentRunning {
		writeError(w, http.StatusConflict, "this deployment has already finished")
		return
	}
	who := "someone"
	if id := identityFrom(r); id != nil {
		who = id.Name
	}
	if d.Spec.Cloudflare != nil {
		// Built by this server rather than an agent: stop the build. Its work stops at the next check.
		if cancel, ok := s.cfRuns.Load(d.ID); ok {
			cancel.(context.CancelFunc)()
		}
	} else if _, err := s.store.CancelCommand(r.Context(), d.CommandID); err != nil {
		s.internal(w, err)
		return
	}
	if err := s.store.FinishDeployment(r.Context(), d.ID, false, "Cancelled by "+who); err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), d.DeviceID, "warning", "deployment.cancelled", fmt.Sprintf("Deployment %s cancelled by %s", d.ID, who))
	cur, err := s.store.GetDeployment(r.Context(), d.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cur)
}
