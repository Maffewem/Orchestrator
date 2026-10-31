package server

import (
	"context"
	"fmt"
	"time"

	"github.com/Maffewem/Orchestrator/internal/store"
)

// pollRepos deploys GitHub projects whose branch head has moved since the last
// deployment. It covers repos where a webhook can't reach this server.
func (s *Server) pollRepos(ctx context.Context) {
	if s.githubToken(ctx) == "" {
		return
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		s.log.Error("poll: list projects", "err", err)
		return
	}
	type key struct{ repo, branch string }
	heads := map[key]*store.Commit{}
	for _, p := range projects {
		src := p.Source
		if !src.FromRepo() {
			continue
		}
		if (src.Type == store.SourceGitHub || src.Type == store.SourceCloudflare) && s.previewSettings(ctx, p.ID).Enabled {
			s.syncPreviews(ctx, p)
		}
		if !src.AutoDeploy {
			continue
		}
		k := key{src.Repo, src.Branch}
		head, ok := heads[k]
		if !ok {
			head, err = s.latestCommit(ctx, src.Repo, src.Branch)
			if err != nil {
				s.log.Warn("poll: latest commit", "repo", src.Repo, "branch", src.Branch, "err", err)
			}
			heads[k] = head // nil on error: skipped, retried next round
		}
		if head == nil {
			continue
		}
		s.pollProject(ctx, p, head)
	}
}

func (s *Server) pollProject(ctx context.Context, p *store.Project, head *store.Commit) {
	s.pollMu.Lock()
	seen, known := s.polled[p.ID]
	s.pollMu.Unlock()
	if !known {
		// First look since startup: baseline on the newest deployment that wasn't a retry
		// of an older commit, so a restart doesn't redeploy what is already live.
		ds, err := s.store.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID, Limit: 20})
		if err != nil {
			s.log.Error("poll: list deployments", "err", err)
			return
		}
		for _, d := range ds {
			if d.Trigger != store.TriggerRetry && d.Trigger != store.TriggerRollback && d.Commit != nil {
				seen = d.Commit.SHA
				break
			}
		}
	}
	if seen == head.SHA {
		s.pollMu.Lock()
		s.polled[p.ID] = seen
		s.pollMu.Unlock()
		return
	}
	if seen != "" && (len(p.Source.Include) > 0 || len(p.Source.Exclude) > 0) &&
		!wants(p.Source.Watch, s.compareFiles(ctx, p.Source.Repo, seen, head.SHA)) {
		s.pollMu.Lock()
		s.polled[p.ID] = head.SHA
		s.pollMu.Unlock()
		return // no watched path changed
	}
	d, err := s.deploy(ctx, p, head, store.TriggerPoll)
	s.pollMu.Lock()
	s.polled[p.ID] = head.SHA // don't hammer a failing deploy; the next new commit tries again
	s.pollMu.Unlock()
	if err != nil {
		s.log.Warn("poll deploy", "project", p.Name, "err", err)
		s.event(ctx, p.DeviceID, "error", "deployment.rejected",
			fmt.Sprintf("New commit on %s could not deploy %s: %v", head.Branch, p.Name, err))
		return
	}
	s.log.Info("poll: deploying new commit", "project", p.Name, "sha", head.SHA[:7], "deployment", d.ID)
}

func (s *Server) pollLoop(ctx context.Context) {
	if s.cfg.PollInterval <= 0 {
		return
	}
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pollRepos(ctx)
		}
	}
}
