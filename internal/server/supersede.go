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
	// Only deployments queued before newer are superseded. If newer is no longer in the queue,
	// an even newer push has already superseded it, and this call must not touch anything:
	// without this check, every deployment queued after it (including the newest) would be
	// treated as older and cancelled.
	self := -1
	for i, d := range queued {
		if d.ID == newer.ID {
			self = i
			break
		}
	}
	for _, old := range queued[:max(self, 0)] {
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
