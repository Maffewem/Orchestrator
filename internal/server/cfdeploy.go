package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Steps of a Cloudflare deployment, matching api.StepsFor.
const (
	cfStepClone = iota
	cfStepInstall
	cfStepBuild
	cfStepUpload
	cfStepVerify
)

const (
	cfMaxConcurrentBuilds = 2
	cfBuildTimeout        = 30 * time.Minute
	cfMaxSourceBytes      = 1 << 30
)

var (
	cfNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	cfDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// cfNameOf is the Pages project or Worker script a project deploys to.
func cfNameOf(p *store.Project) string {
	if t := p.Source.Cloudflare; t != nil && t.Name != "" {
		return t.Name
	}
	return serviceName(p.Name)
}

// cfPreviewScript names the Worker script that carries a preview, within Cloudflare's 63-character limit.
func cfPreviewScript(name, slug string) string {
	n := name + "-" + slug
	if len(n) > 63 {
		n = strings.TrimRight(n[:63], "-")
	}
	return n
}

// deployCloudflare records a deployment of p at commit and starts building it in the background.
// A non-nil pv deploys a preview of its branch instead of production.
func (s *Server) deployCloudflare(ctx context.Context, p *store.Project, commit *store.Commit, trigger string, pv *store.Preview) (*store.Deployment, error) {
	if !s.cfDeployReady(ctx) {
		return nil, errors.New("connect Cloudflare Pages & Workers in Settings first: it needs an account ID and an API token")
	}
	if p.Source.Repo == "" {
		return nil, errors.New("this project was imported from Cloudflare and has no repository yet: add one in its settings to deploy from here")
	}
	if commit == nil {
		return nil, errors.New("no commit to build")
	}
	src := p.Source
	if pv != nil {
		src = previewSource(src)
	}
	t := src.Cloudflare
	if t == nil {
		return nil, errors.New("this project has no Cloudflare settings")
	}
	env := store.EnvAssignments(p.Runtime.Env)
	for _, e := range env {
		if store.IsSealed(e) {
			return nil, errors.New("environment values can't be decrypted: the server's secret key has changed since they were saved. Re-enter them in the project settings")
		}
	}
	cs := &api.CloudflareSpec{
		Kind: t.Kind, Name: cfNameOf(p), Repo: src.Repo, SHA: commit.SHA, Context: src.Context,
		Branch: src.Branch, ProductionBranch: src.Branch, Output: t.Output, CompatibilityDate: t.CompatibilityDate,
	}
	if src.Node != nil {
		cs.Build = *src.Node
	}
	pvID := ""
	if pv != nil {
		pvID = pv.ID
		cs.Branch = pv.Branch
		if t.Kind == api.CloudflareWorker {
			cs.Name = cfPreviewScript(cs.Name, pv.Slug)
		}
	}
	spec := api.DeploySpec{Service: p.Name, Env: env, Cloudflare: cs}
	d, err := s.store.CreateDeployment(ctx, store.NewDeployment{
		ProjectID: p.ID, Trigger: trigger, Commit: commit, Spec: spec, PreviewID: pvID, Local: true,
	})
	if err != nil {
		return nil, err
	}
	what := "Deployment of " + p.Name
	if pv != nil {
		what = "Preview of " + p.Name
	}
	s.event(ctx, "", "info", "deployment.queued", fmt.Sprintf("%s to Cloudflare queued for %s@%.7s (%s)", what, commit.Branch, commit.SHA, trigger))
	go s.runCloudflare(d.ID)
	return d, nil
}

func (s *Server) cfLock(key string) *sync.Mutex {
	m, _ := s.cfLocks.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// runCloudflare carries out a queued Cloudflare deployment from start to finish.
func (s *Server) runCloudflare(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), cfBuildTimeout)
	defer cancel()
	s.cfRuns.Store(id, cancel)
	defer s.cfRuns.Delete(id)

	dep, err := s.store.GetDeployment(ctx, id)
	if err != nil || dep.Spec.Cloudflare == nil {
		s.log.Error("cloudflare deploy: load", "deployment", id, "err", err)
		return
	}
	cs := dep.Spec.Cloudflare
	// One build per target at a time, and a few at once overall.
	l := s.cfLock(cs.Kind + "/" + cs.Name)
	l.Lock()
	defer l.Unlock()
	select {
	case s.cfSem <- struct{}{}:
		defer func() { <-s.cfSem }()
	case <-ctx.Done():
	}

	var remoteID, deployURL string
	if ctx.Err() == nil {
		remoteID, deployURL, err = s.cfExecute(ctx, dep)
	} else {
		err = ctx.Err()
	}

	bg := context.WithoutCancel(ctx)
	if cur, e := s.store.GetDeployment(bg, id); e == nil && (cur.Status == store.DeploymentFailed || cur.Status == store.DeploymentSucceeded) {
		return // cancelled while it ran
	}
	if remoteID != "" || deployURL != "" {
		s.store.SetDeploymentRemote(bg, id, remoteID, deployURL)
	}
	msg := ""
	if err != nil {
		msg = err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			msg = "timed out after " + cfBuildTimeout.String()
		}
	}
	if e := s.store.FinishDeployment(bg, id, err == nil, msg); e != nil {
		s.log.Error("cloudflare deploy: finish", "deployment", id, "err", e)
		return
	}
	if err == nil {
		s.event(bg, "", "info", "deployment.succeeded", fmt.Sprintf("%s deployed to Cloudflare %s", dep.Spec.Service, cs.Kind))
	} else {
		s.event(bg, "", "error", "deployment.failed", fmt.Sprintf("%s failed to deploy to Cloudflare %s: %s", dep.Spec.Service, cs.Kind, msg))
	}
	if p, e := s.store.GetProject(bg, dep.ProjectID); e == nil && dep.PreviewID == "" {
		s.cfPollProject(bg, p)
	}
}

// cfSteps reports progress of a deployment through the same step timeline agents use.
type cfSteps struct {
	s  *Server
	id string
}

func (c cfSteps) report(idx int, status string, lines ...string) {
	u := api.StepUpdate{Status: status}
	for _, l := range lines {
		u.Lines = append(u.Lines, api.LogLine{Time: time.Now(), Text: l})
	}
	if err := c.s.store.UpdateStep(context.Background(), "", c.id, idx, u); err != nil {
		c.s.log.Warn("cloudflare deploy: step", "deployment", c.id, "err", err)
	}
}

func (c cfSteps) start(idx int)                    { c.report(idx, api.StepRunning) }
func (c cfSteps) logf(idx int, f string, a ...any) { c.report(idx, "", fmt.Sprintf(f, a...)) }
func (c cfSteps) done(idx int, lines ...string)    { c.report(idx, api.StepSucceeded, lines...) }
func (c cfSteps) skip(idx int, why string)         { c.report(idx, api.StepSkipped, why) }
func (c cfSteps) fail(idx int, err error)          { c.report(idx, api.StepFailed, err.Error()) }
func (c cfSteps) writer(idx int) *stepWriter       { return &stepWriter{steps: c, idx: idx} }

// stepWriter turns command output into log lines, sent in batches.
type stepWriter struct {
	steps cfSteps
	idx   int
	mu    sync.Mutex
	part  string
	lines []string
	last  time.Time
}

func (w *stepWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.part += string(b)
	for {
		i := strings.IndexAny(w.part, "\r\n")
		if i < 0 {
			break
		}
		if line := w.part[:i]; strings.TrimSpace(line) != "" {
			w.lines = append(w.lines, line)
		}
		w.part = w.part[i+1:]
	}
	if len(w.lines) >= 40 || (len(w.lines) > 0 && time.Since(w.last) > 400*time.Millisecond) {
		w.flushLocked()
	}
	return len(b), nil
}

func (w *stepWriter) flushLocked() {
	if len(w.lines) > 0 {
		w.steps.report(w.idx, "", w.lines...)
	}
	w.lines, w.last = nil, time.Now()
}

func (w *stepWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if strings.TrimSpace(w.part) != "" {
		w.lines = append(w.lines, w.part)
		w.part = ""
	}
	w.flushLocked()
}

// cfExecute builds the revision and uploads it. It returns the Cloudflare deployment ID and URL.
func (s *Server) cfExecute(ctx context.Context, dep *store.Deployment) (remoteID, deployURL string, err error) {
	cs := dep.Spec.Cloudflare
	steps := cfSteps{s: s, id: dep.ID}
	tok, acct := s.cfDeployCreds(ctx)
	if tok == "" || acct == "" {
		return "", "", errors.New("Cloudflare Pages & Workers is not connected any more")
	}

	base := s.cfg.BuildDir
	if base == "" {
		base = filepath.Join(os.TempDir(), "orchestrator-builds")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", "", err
	}
	work, err := os.MkdirTemp(base, "build-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(work)
	srcDir := filepath.Join(work, "src")

	// Cloning
	steps.start(cfStepClone)
	steps.logf(cfStepClone, "Fetching %s at %.7s", cs.Repo, cs.SHA)
	n, err := s.cfFetchSource(ctx, cs.Repo, cs.SHA, srcDir)
	if err != nil {
		steps.fail(cfStepClone, err)
		return "", "", err
	}
	steps.done(cfStepClone, fmt.Sprintf("Unpacked %d files", n))
	root := srcDir
	if cs.Context != "" {
		root = filepath.Join(srcDir, filepath.FromSlash(cs.Context))
		if st, e := os.Stat(root); e != nil || !st.IsDir() {
			err := fmt.Errorf("the root directory %q is not in the repository", cs.Context)
			steps.fail(cfStepClone, err)
			return "", "", err
		}
	}

	env := s.cfBuildEnv(work, cs, dep.Spec.Env)
	pkg := readPackageJSON(root)

	// Installing
	steps.start(cfStepInstall)
	if cmd := installCommand(root, cs.Build.Install, pkg != nil); cmd == "" {
		steps.skip(cfStepInstall, "No package.json, nothing to install")
	} else if err := s.cfRun(ctx, steps, cfStepInstall, root, env, cmd); err != nil {
		steps.fail(cfStepInstall, err)
		return "", "", err
	} else {
		steps.done(cfStepInstall)
	}

	// Building
	steps.start(cfStepBuild)
	if cmd := buildCommand(cs.Build.Build, pkg); cmd == "" {
		steps.skip(cfStepBuild, "No build command and no build script in package.json")
	} else if err := s.cfRun(ctx, steps, cfStepBuild, root, env, cmd); err != nil {
		steps.fail(cfStepBuild, err)
		return "", "", err
	} else {
		steps.done(cfStepBuild)
	}

	// Uploading
	steps.start(cfStepUpload)
	remoteID, deployURL, err = s.cfUpload(ctx, steps, tok, acct, cs, dep, root)
	if err != nil {
		steps.fail(cfStepUpload, err)
		return "", "", err
	}
	steps.done(cfStepUpload)

	// Verifying
	steps.start(cfStepVerify)
	if deployURL == "" {
		steps.skip(cfStepVerify, "This Worker has no workers.dev address to check")
		return remoteID, deployURL, nil
	}
	status, err := s.cfVerify(ctx, deployURL)
	if err != nil {
		steps.fail(cfStepVerify, err)
		return remoteID, deployURL, err
	}
	steps.done(cfStepVerify, fmt.Sprintf("GET %s answered %s", deployURL, status))
	return remoteID, deployURL, nil
}

func (s *Server) cfUpload(ctx context.Context, steps cfSteps, tok, acct string, cs *api.CloudflareSpec, dep *store.Deployment, root string) (remoteID, deployURL string, err error) {
	logf := func(f string, a ...any) { steps.logf(cfStepUpload, f, a...) }
	var sha, msg string
	sha = cs.SHA
	if dep.Commit != nil {
		msg = dep.Commit.Message
	}
	switch cs.Kind {
	case api.CloudflarePages:
		out, err := pagesOutput(root, cs.Output)
		if err != nil {
			return "", "", err
		}
		logf("Uploading %s to Pages project %s (branch %s)", displayDir(root, out), cs.Name, cs.Branch)
		files, err := readSite(out)
		if err != nil {
			return "", "", err
		}
		if created, err := s.cfEnsurePagesProject(ctx, tok, acct, cs.Name, cs.ProductionBranch); err != nil {
			return "", "", fmt.Errorf("preparing the Pages project: %w", err)
		} else if created {
			logf("Created Pages project %s", cs.Name)
		}
		id, u, err := s.cfDeployPages(ctx, tok, acct, pagesDeployInput{Name: cs.Name, Branch: cs.Branch, SHA: sha, Message: msg, Files: files}, logf)
		if err != nil {
			return "", "", err
		}
		logf("Deployment %s is live at %s", id, u)
		return id, u, nil
	case api.CloudflareWorker:
		entry, err := workerEntry(root, cs.Output)
		if err != nil {
			return "", "", err
		}
		code, err := os.ReadFile(entry)
		if err != nil {
			return "", "", err
		}
		compat := cs.CompatibilityDate
		if compat == "" {
			compat = time.Now().UTC().Format("2006-01-02")
		}
		module := filepath.Base(entry)
		logf("Uploading %s (%d KB) as Worker %s", displayDir(root, entry), (len(code)+1023)/1024, cs.Name)
		if err := s.cfUploadWorker(ctx, tok, acct, cs.Name, module, code, compat); err != nil {
			return "", "", fmt.Errorf("uploading the Worker: %w", err)
		}
		u := s.cfWorkerURL(ctx, tok, acct, cs.Name)
		if u != "" {
			logf("Worker is live at %s", u)
		} else {
			logf("Worker uploaded. The account has no workers.dev subdomain, so serve it from a route")
		}
		var rid string
		if ds, err := s.cfWorkerDeployments(ctx, tok, acct, cs.Name); err == nil && len(ds) > 0 {
			rid = ds[0].ID
		}
		return rid, u, nil
	}
	return "", "", fmt.Errorf("unknown Cloudflare kind %q", cs.Kind)
}

func displayDir(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil && r != "." {
		return filepath.ToSlash(r)
	}
	return "the project root"
}

// cfVerify asks the new deployment's URL for a page until something other than a server error comes back.
func (s *Server) cfVerify(ctx context.Context, u string) (string, error) {
	wait := s.cfWait
	if wait == 0 {
		wait = 5 * time.Second
	}
	var last string
	for i := 0; i < 12; i++ {
		if i > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		target := u
		if s.cfVerifyURL != nil {
			target = s.cfVerifyURL(u)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		c := http.Client{Timeout: 10 * time.Second}
		resp, err := c.Do(req)
		if err != nil {
			last = err.Error()
			continue
		}
		resp.Body.Close()
		if resp.StatusCode < 500 {
			return resp.Status, nil
		}
		last = resp.Status
	}
	return "", fmt.Errorf("uploaded, but %s did not answer (%s)", u, last)
}

// rollbackCloudflare makes an earlier production deployment live again. A Pages site is
// re-promoted on Cloudflare, so nothing is rebuilt. A Worker has no such step here, so its
// commit is built and uploaded again.
func (s *Server) rollbackCloudflare(ctx context.Context, p *store.Project, old *store.Deployment) (*store.Deployment, error) {
	cs := old.Spec.Cloudflare
	if cs == nil || cs.Kind != api.CloudflarePages || old.RemoteID == "" || !s.cfDeployReady(ctx) {
		return s.deployCloudflare(ctx, p, old.Commit, store.TriggerRollback, nil)
	}
	tok, acct := s.cfDeployCreds(ctx)
	spec := old.Spec
	d, err := s.store.CreateDeployment(ctx, store.NewDeployment{
		ProjectID: p.ID, Trigger: store.TriggerRollback, Commit: old.Commit, Spec: spec, Local: true,
	})
	if err != nil {
		return nil, err
	}
	steps := cfSteps{s: s, id: d.ID}
	for _, i := range []int{cfStepClone, cfStepInstall, cfStepBuild} {
		steps.skip(i, "Nothing to build: the earlier deployment is promoted as it is")
	}
	steps.start(cfStepUpload)
	steps.logf(cfStepUpload, "Rolling %s back to deployment %s", cs.Name, old.RemoteID)
	rolled, err := s.cfRollbackPages(ctx, tok, acct, cs.Name, old.RemoteID)
	if err != nil {
		steps.fail(cfStepUpload, err)
		s.store.FinishDeployment(ctx, d.ID, false, err.Error())
		s.event(ctx, "", "error", "deployment.failed", fmt.Sprintf("Rollback of %s failed: %s", p.Name, err))
		return s.store.GetDeployment(ctx, d.ID)
	}
	remote, u := old.RemoteID, old.URL
	if rolled.ID != "" {
		remote = rolled.ID
	}
	if rolled.URL != "" {
		u = rolled.URL
	}
	s.store.SetDeploymentRemote(ctx, d.ID, remote, u)
	steps.done(cfStepUpload, "Deployment "+remote+" is live again")
	steps.skip(cfStepVerify, "Skipped for a rollback")
	s.store.FinishDeployment(ctx, d.ID, true, "")
	s.event(ctx, "", "info", "deployment.succeeded", fmt.Sprintf("%s rolled back to an earlier Cloudflare Pages deployment", p.Name))
	go s.cfPollProject(context.WithoutCancel(ctx), p)
	return s.store.GetDeployment(ctx, d.ID)
}
