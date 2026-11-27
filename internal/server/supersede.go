package server

import (
	"context"

	"github.com/Maffewem/Orchestrator/internal/store"
)

// supersedeQueued drops production deployments of the same project that were queued by an
// earlier push or poll and have not started, now that newer is queued. A build of an older
// commit would only be replaced moments later, and on a small Pi it would delay the newer one.
// A deployment that is already running is never interrupted.
func (s *Server) supersedeQueued(ctx context.Context, newer *store.Deployment) {
	if newer.PreviewID != "" || newer.Spec.Cloudflare != nil {
		return
	}
	queued, err := s.store.QueuedPushDeployments(ctx, newer.ProjectID)
	if err != nil {
		s.log.Warn("supersede: list queued", "err", err)
		return
	}
	for _, old := range queued {
		if old.ID == newer.ID {
			// The list is in queue order: everything after this was queued later, possibly by a
			// push handled at the same moment, and must survive.
			break
		}
		if old.Spec.Cloudflare != nil || old.DeviceID != newer.DeviceID {
			continue
		}
		if _, err := s.store.CancelCommand(ctx, old.CommandID); err != nil {
			s.log.Warn("supersede: cancel command", "err", err)
			continue
		}
		if err := s.store.FinishDeployment(ctx, old.ID, false, "Superseded by a newer push before it started"); err != nil {
			s.log.Warn("supersede: finish", "err", err)
		}
	}
}
