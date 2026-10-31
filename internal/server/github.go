package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

const (
	settingGitHubToken   = "github_token"
	settingWebhookSecret = "github_webhook_secret"
)

// githubRemote is the clone URL prefix that credentials get injected into at dispatch time.
const githubRemote = "https://github.com/"

func (s *Server) githubToken(ctx context.Context) string {
	tok, _ := s.store.GetSetting(ctx, settingGitHubToken)
	return tok
}

// handleGitHubWebhook deploys projects that track the pushed branch.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 25<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read body")
		return
	}
	secret, err := s.store.GetSetting(r.Context(), settingWebhookSecret)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !validSignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	switch r.Header.Get("X-GitHub-Event") {
	case "ping":
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	case "push":
	case "pull_request":
		var pr struct {
			Action     string `json:"action"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &pr); err != nil {
			writeError(w, http.StatusBadRequest, "invalid pull request payload")
			return
		}
		switch pr.Action {
		case "opened", "reopened", "synchronize", "closed", "ready_for_review", "edited":
			n := s.handlePullRequestEvent(r.Context(), pr.Repository.FullName)
			writeJSON(w, http.StatusAccepted, map[string]int{"projects": n})
		default:
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
		}
		return
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}

	var ev struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Deleted    bool   `json:"deleted"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Commits []struct {
			Added    []string `json:"added"`
			Modified []string `json:"modified"`
			Removed  []string `json:"removed"`
		} `json:"commits"`
		HeadCommit *struct {
			ID      string `json:"id"`
			Message string `json:"message"`
			URL     string `json:"url"`
			Author  struct {
				Name     string `json:"name"`
				Username string `json:"username"`
			} `json:"author"`
		} `json:"head_commit"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		writeError(w, http.StatusBadRequest, "invalid push payload")
		return
	}
	branch, isBranch := strings.CutPrefix(ev.Ref, "refs/heads/")
	if !isBranch || ev.Deleted || ev.HeadCommit == nil {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}
	author := ev.HeadCommit.Author.Username
	if author == "" {
		author = ev.HeadCommit.Author.Name
	}
	commit := &store.Commit{
		Repo:    ev.Repository.FullName,
		Branch:  branch,
		SHA:     ev.HeadCommit.ID,
		Message: ev.HeadCommit.Message,
		Author:  author,
		URL:     ev.HeadCommit.URL,
	}

	projects, err := s.store.ProjectsForPush(r.Context(), commit.Repo, branch)
	if err != nil {
		s.internal(w, err)
		return
	}
	// GitHub lists at most 20 commits per push; beyond that the file list is unknown.
	var files []string
	if n := len(ev.Commits); n > 0 && n < 20 {
		files = []string{}
		for _, c := range ev.Commits {
			files = append(append(append(files, c.Added...), c.Modified...), c.Removed...)
		}
	}
	ids := []string{}
	for _, p := range projects {
		if !wants(p.Source.Watch, files) {
			s.log.Info("push: no watched paths changed", "project", p.Name)
			continue
		}
		d, err := s.deploy(r.Context(), p, commit, store.TriggerPush)
		if err != nil {
			s.log.Warn("push deploy", "project", p.Name, "err", err)
			s.event(r.Context(), p.DeviceID, "error", "deployment.rejected",
				fmt.Sprintf("Push to %s could not deploy %s: %v", branch, p.Name, err))
			continue
		}
		ids = append(ids, d.ID)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"deployments": ids})
}

func validSignature(secret string, body []byte, header string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok || secret == "" {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// latestCommit asks GitHub for the head of a branch.
func (s *Server) latestCommit(ctx context.Context, repo, branch string) (*store.Commit, error) {
	var out struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
		Commit  struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"commit"`
		Author *struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	path := fmt.Sprintf("/repos/%s/commits/%s", repo, url.PathEscape(branch))
	if err := s.githubGet(ctx, path, &out); err != nil {
		return nil, err
	}
	author := out.Commit.Author.Name
	if out.Author != nil && out.Author.Login != "" {
		author = out.Author.Login
	}
	return &store.Commit{Repo: repo, Branch: branch, SHA: out.SHA, Message: out.Commit.Message, Author: author, URL: out.HTMLURL}, nil
}

type githubRepo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	HTMLURL       string `json:"html_url"`
}

func (s *Server) handleListGitHubRepos(w http.ResponseWriter, r *http.Request) {
	if s.githubToken(r.Context()) == "" {
		writeError(w, http.StatusPreconditionFailed, "connect GitHub in Settings first")
		return
	}
	var repos []githubRepo
	if err := s.githubGet(r.Context(), "/user/repos?per_page=100&sort=pushed", &repos); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, repos)
}

func (s *Server) githubGet(ctx context.Context, path string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.githubAPI+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if tok := s.githubToken(ctx); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("github: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("github: %s (%s)", resp.Status, e.Message)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// specFor turns a project (and, for GitHub projects, a commit) into what the agent runs.
func specFor(p *store.Project, commit *store.Commit) (api.DeploySpec, error) {
	restart := p.Runtime.Restart
	if restart == "" {
		restart = "unless-stopped"
	}
	env := store.EnvAssignments(p.Runtime.Env)
	for _, e := range env {
		if store.IsSealed(e) {
			return api.DeploySpec{}, errors.New("environment values can't be decrypted: the server's secret key has changed since they were saved. Re-enter them in the project settings")
		}
	}
	spec := api.DeploySpec{
		Service: serviceOf(p),
		Ports:   p.Runtime.Ports,
		Env:     env,
		Command: p.Runtime.Command,
		Restart: restart,
		Volumes: p.Runtime.Volumes,
		Network: p.Runtime.Network,
		Labels:  map[string]string{},
	}
	for k, v := range p.Runtime.Labels {
		spec.Labels[k] = v
	}
	spec.Labels["orchestrator.project"] = p.ID
	switch p.Source.Type {
	case store.SourceImage:
		if p.Source.Image == "" {
			return spec, errors.New("project has no image")
		}
		spec.Image = p.Source.Image
	case store.SourceCompose:
		if strings.TrimSpace(p.Source.Compose) == "" {
			return spec, errors.New("project has no compose file")
		}
		if err := checkComposeVars(p.Source.Compose, env); err != nil {
			return spec, err
		}
		spec.Compose = &api.ComposeSpec{YAML: p.Source.Compose, Env: env}
		if p.Source.Repo != "" {
			if commit == nil || commit.SHA == "" {
				return spec, errors.New("no commit to deploy")
			}
			spec.Compose.Remote = githubRemote + p.Source.Repo + ".git#" + commit.SHA
		}
		spec.Service = p.Name
		spec.Ports, spec.Env, spec.Volumes, spec.Command, spec.Network, spec.Labels = nil, nil, nil, nil, "", nil
	case store.SourceGitHub:
		if commit == nil || commit.SHA == "" {
			return spec, errors.New("no commit to build")
		}
		dir := strings.Trim(p.Source.Context, "/")
		remote := githubRemote + p.Source.Repo + ".git#" + commit.SHA
		if dir != "" && dir != "." {
			remote += ":" + dir
		}
		short := commit.SHA
		if len(short) > 7 {
			short = short[:7]
		}
		spec.Image = "orchestrator/" + spec.Service + ":" + short
		spec.Build = &api.BuildSpec{Remote: remote, Dockerfile: p.Source.Dockerfile, Node: p.Source.Node}
	default:
		return spec, fmt.Errorf("unknown source type %q", p.Source.Type)
	}
	return spec, nil
}

// withCredentials adds the GitHub token to build remotes as commands leave for an agent,
// so the token is never stored with deployments.
func withCredentials(cmds []api.Command, token string) {
	if token == "" {
		return
	}
	for i := range cmds {
		c := &cmds[i]
		if c.Deploy == nil {
			continue
		}
		spec := *c.Deploy
		switch {
		case spec.Build != nil && strings.HasPrefix(spec.Build.Remote, githubRemote):
			build := *spec.Build
			build.Remote = withToken(build.Remote, token)
			spec.Build = &build
		case spec.Compose != nil && strings.HasPrefix(spec.Compose.Remote, githubRemote):
			cs := *spec.Compose
			cs.Remote = withToken(cs.Remote, token)
			spec.Compose = &cs
		default:
			continue
		}
		c.Deploy = &spec
	}
}

func withToken(remote, token string) string {
	return "https://x-access-token:" + url.PathEscape(token) + "@github.com/" + strings.TrimPrefix(remote, githubRemote)
}

// repoFile reads one file of a repository at a commit through the contents API.
func (s *Server) repoFile(ctx context.Context, repo, file, ref string) (string, error) {
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	parts := strings.Split(file, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	path := fmt.Sprintf("/repos/%s/contents/%s?ref=%s", repo, strings.Join(parts, "/"), url.QueryEscape(ref))
	if err := s.githubGet(ctx, path, &out); err != nil {
		return "", fmt.Errorf("reading %s from %s: %w", file, repo, err)
	}
	if out.Encoding != "base64" {
		return "", fmt.Errorf("%s is not a file", file)
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// serviceOf is the name of the container a project runs as: the project name, unless the
// project adopted a container that already had a different one.
func serviceOf(p *store.Project) string {
	if p.Runtime.Container != "" {
		return p.Runtime.Container
	}
	return serviceName(p.Name)
}

// serviceName makes a Docker-safe container name from a project name.
func serviceName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "service"
	}
	return out
}
