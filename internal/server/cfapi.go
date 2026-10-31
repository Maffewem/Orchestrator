package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strings"
	"time"

	"lukechampine.com/blake3"
)

// Settings for deploying to Cloudflare Pages and Workers. The token may be the Cloudflare token
// from Settings (which also manages DNS and tunnels) or a narrower one just for deployments.
const (
	settingCFAccount     = "cloudflare_account_id"
	settingCFDeployToken = "cloudflare_deploy_token"
)

// cfDeployCreds returns the token and account ID used to deploy and monitor.
func (s *Server) cfDeployCreds(ctx context.Context) (token, account string) {
	account, _ = s.store.GetSetting(ctx, settingCFAccount)
	if t, _ := s.store.GetSetting(ctx, settingCFDeployToken); t != "" {
		return t, account
	}
	return s.cloudflareToken(ctx), account
}

func (s *Server) cfDeployReady(ctx context.Context) bool {
	t, a := s.cfDeployCreds(ctx)
	return t != "" && a != ""
}

// cfNotFound reports whether err is Cloudflare saying the thing does not exist.
func cfNotFound(err error) bool {
	var ce *cfError
	return errors.As(err, &ce) && ce.Status == http.StatusNotFound
}

// --- Pages ---

type cfPagesProject struct {
	Name             string             `json:"name"`
	Subdomain        string             `json:"subdomain"`
	Domains          []string           `json:"domains"`
	ProductionBranch string             `json:"production_branch"`
	Latest           *cfPagesDeployment `json:"latest_deployment"`
	Canonical        *cfPagesDeployment `json:"canonical_deployment"`
}

type cfPagesDeployment struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	Environment string `json:"environment"`
	CreatedOn   string `json:"created_on"`
	Stage       struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"latest_stage"`
}

func (s *Server) cfGetPagesProject(ctx context.Context, tok, acct, name string) (*cfPagesProject, error) {
	var p cfPagesProject
	if err := s.cfCall(ctx, tok, http.MethodGet, fmt.Sprintf("/accounts/%s/pages/projects/%s", acct, url.PathEscape(name)), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// cfEnsurePagesProject creates the Pages project on first use.
func (s *Server) cfEnsurePagesProject(ctx context.Context, tok, acct, name, branch string) (created bool, err error) {
	if _, err = s.cfGetPagesProject(ctx, tok, acct, name); err == nil {
		return false, nil
	} else if !cfNotFound(err) {
		return false, err
	}
	body := map[string]any{"name": name, "production_branch": branch}
	return true, s.cfCall(ctx, tok, http.MethodPost, fmt.Sprintf("/accounts/%s/pages/projects", acct), body, nil)
}

// pagesFile is one file of a Pages site.
type pagesFile struct {
	Path    string // slash-separated, relative to the site root, without a leading slash
	Content []byte
}

// pagesHash is how Pages names an asset: the first 32 hex characters of the BLAKE3 hash of the
// base64 content followed by the extension (without its dot), as Wrangler computes it.
func pagesHash(f pagesFile) string {
	ext := strings.TrimPrefix(path.Ext(f.Path), ".")
	sum := blake3.Sum256([]byte(base64.StdEncoding.EncodeToString(f.Content) + ext))
	return hex.EncodeToString(sum[:])[:32]
}

// pagesSpecial are files Pages reads as configuration instead of serving them as assets.
var pagesSpecial = map[string]bool{"_headers": true, "_redirects": true, "_routes.json": true, "_worker.js": true}

const (
	pagesMaxFiles    = 20000
	pagesMaxFileSize = 25 << 20
	pagesBucketBytes = 40 << 20
)

type pagesDeployInput struct {
	Name, Branch, SHA, Message string
	Files                      []pagesFile
}

// cfDeployPages uploads the site's assets, then creates the deployment from them. It returns
// the deployment's ID and URL.
func (s *Server) cfDeployPages(ctx context.Context, tok, acct string, in pagesDeployInput, logf func(string, ...any)) (id, deployURL string, err error) {
	var assets []pagesFile
	special := map[string][]byte{}
	for _, f := range in.Files {
		if pagesSpecial[f.Path] {
			special[f.Path] = f.Content
			continue
		}
		if len(f.Content) > pagesMaxFileSize {
			return "", "", fmt.Errorf("%s is %d MB; Pages files can be at most 25 MB", f.Path, len(f.Content)>>20)
		}
		assets = append(assets, f)
	}
	if len(assets) > pagesMaxFiles {
		return "", "", fmt.Errorf("%d files; a Pages deployment can have at most %d", len(assets), pagesMaxFiles)
	}
	if len(assets) == 0 && special["_worker.js"] == nil {
		return "", "", errors.New("the output directory has no files to upload")
	}

	manifest := map[string]string{}
	byHash := map[string]pagesFile{}
	for _, f := range assets {
		h := pagesHash(f)
		manifest["/"+f.Path] = h
		byHash[h] = f
	}
	hashes := make([]string, 0, len(byHash))
	for h := range byHash {
		hashes = append(hashes, h)
	}

	if len(hashes) > 0 {
		var tokenResp struct {
			JWT string `json:"jwt"`
		}
		if err := s.cfCall(ctx, tok, http.MethodGet, fmt.Sprintf("/accounts/%s/pages/projects/%s/upload-token", acct, url.PathEscape(in.Name)), nil, &tokenResp); err != nil {
			return "", "", fmt.Errorf("getting an upload token: %w", err)
		}
		var missing []string
		if err := s.cfCall(ctx, tokenResp.JWT, http.MethodPost, "/pages/assets/check-missing", map[string]any{"hashes": hashes}, &missing); err != nil {
			return "", "", fmt.Errorf("checking which files are already uploaded: %w", err)
		}
		logf("%d files, %d new", len(assets), len(missing))

		type upload struct {
			Key      string         `json:"key"`
			Value    string         `json:"value"`
			Metadata map[string]any `json:"metadata"`
			Base64   bool           `json:"base64"`
		}
		var bucket []upload
		size := 0
		flush := func() error {
			if len(bucket) == 0 {
				return nil
			}
			err := s.cfRaw(ctx, 2*time.Minute, tokenResp.JWT, http.MethodPost, "/pages/assets/upload", "application/json", jsonReader(bucket), nil)
			bucket, size = nil, 0
			return err
		}
		for _, h := range missing {
			f, ok := byHash[h]
			if !ok {
				continue
			}
			ct := mime.TypeByExtension(path.Ext(f.Path))
			if ct == "" {
				ct = http.DetectContentType(f.Content)
			}
			bucket = append(bucket, upload{Key: h, Value: base64.StdEncoding.EncodeToString(f.Content), Metadata: map[string]any{"contentType": ct}, Base64: true})
			if size += len(f.Content); size >= pagesBucketBytes {
				if err := flush(); err != nil {
					return "", "", fmt.Errorf("uploading files: %w", err)
				}
			}
		}
		if err := flush(); err != nil {
			return "", "", fmt.Errorf("uploading files: %w", err)
		}
		if err := s.cfCall(ctx, tokenResp.JWT, http.MethodPost, "/pages/assets/upsert-hashes", map[string]any{"hashes": hashes}, nil); err != nil {
			return "", "", fmt.Errorf("recording the uploaded files: %w", err)
		}
	}

	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	mb, _ := json.Marshal(manifest)
	mw.WriteField("manifest", string(mb))
	mw.WriteField("branch", in.Branch)
	mw.WriteField("commit_hash", in.SHA)
	mw.WriteField("commit_message", firstLine(in.Message))
	mw.WriteField("commit_dirty", "false")
	for name, content := range special {
		fw, _ := mw.CreateFormFile(name, name)
		fw.Write(content)
	}
	mw.Close()
	var dep cfPagesDeployment
	err = s.cfRaw(ctx, 2*time.Minute, tok, http.MethodPost, fmt.Sprintf("/accounts/%s/pages/projects/%s/deployments", acct, url.PathEscape(in.Name)), mw.FormDataContentType(), &form, &dep)
	if err != nil {
		return "", "", fmt.Errorf("creating the deployment: %w", err)
	}
	return dep.ID, dep.URL, nil
}

// cfRollbackPages makes an earlier production deployment the live one again.
func (s *Server) cfRollbackPages(ctx context.Context, tok, acct, name, deploymentID string) (*cfPagesDeployment, error) {
	var dep cfPagesDeployment
	p := fmt.Sprintf("/accounts/%s/pages/projects/%s/deployments/%s/rollback", acct, url.PathEscape(name), url.PathEscape(deploymentID))
	if err := s.cfCall(ctx, tok, http.MethodPost, p, nil, &dep); err != nil {
		return nil, err
	}
	return &dep, nil
}

// --- Workers ---

// cfUploadWorker creates or replaces a Worker script made of one ES module.
func (s *Server) cfUploadWorker(ctx context.Context, tok, acct, name, module string, code []byte, compat string) error {
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	meta, _ := json.Marshal(map[string]any{"main_module": module, "compatibility_date": compat})
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="metadata"`)
	h.Set("Content-Type", "application/json")
	pw, _ := mw.CreatePart(h)
	pw.Write(meta)
	h = textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, module, module))
	h.Set("Content-Type", "application/javascript+module")
	pw, _ = mw.CreatePart(h)
	pw.Write(code)
	mw.Close()
	return s.cfRaw(ctx, 2*time.Minute, tok, http.MethodPut, fmt.Sprintf("/accounts/%s/workers/scripts/%s", acct, url.PathEscape(name)), mw.FormDataContentType(), &form, nil)
}

// cfWorkerURL turns on the workers.dev address of a script and returns it, or "" when the
// account has no workers.dev subdomain (the script may still be served on a route).
func (s *Server) cfWorkerURL(ctx context.Context, tok, acct, name string) string {
	var sub struct {
		Subdomain string `json:"subdomain"`
	}
	if err := s.cfCall(ctx, tok, http.MethodGet, fmt.Sprintf("/accounts/%s/workers/subdomain", acct), nil, &sub); err != nil || sub.Subdomain == "" {
		return ""
	}
	s.cfCall(ctx, tok, http.MethodPost, fmt.Sprintf("/accounts/%s/workers/scripts/%s/subdomain", acct, url.PathEscape(name)), map[string]any{"enabled": true}, nil)
	return fmt.Sprintf("https://%s.%s.workers.dev", name, sub.Subdomain)
}

func (s *Server) cfDeleteWorker(ctx context.Context, tok, acct, name string) error {
	err := s.cfCall(ctx, tok, http.MethodDelete, fmt.Sprintf("/accounts/%s/workers/scripts/%s", acct, url.PathEscape(name)), nil, nil)
	if cfNotFound(err) {
		return nil
	}
	return err
}

type cfWorkerDeployment struct {
	ID        string `json:"id"`
	CreatedOn string `json:"created_on"`
}

// cfWorkerDeployments lists a script's deployments, newest first.
func (s *Server) cfWorkerDeployments(ctx context.Context, tok, acct, name string) ([]cfWorkerDeployment, error) {
	var out struct {
		Deployments []cfWorkerDeployment `json:"deployments"`
	}
	if err := s.cfCall(ctx, tok, http.MethodGet, fmt.Sprintf("/accounts/%s/workers/scripts/%s/deployments", acct, url.PathEscape(name)), nil, &out); err != nil {
		return nil, err
	}
	return out.Deployments, nil
}

// cfWorkerStats reads a script's request and error counts for the last 24 hours from the GraphQL analytics API.
type cfWorkerStats struct {
	Requests, Errors, Subrequests int64
	CPUP50, CPUP99                float64 // microseconds
}

func (s *Server) cfWorkerAnalytics(ctx context.Context, tok, acct, script string, now time.Time) (*cfWorkerStats, error) {
	const q = `query($acct: String!, $script: String!, $since: Time!, $until: Time!) {
  viewer { accounts(filter: {accountTag: $acct}) {
    workersInvocationsAdaptive(limit: 1, filter: {scriptName: $script, datetime_geq: $since, datetime_leq: $until}) {
      sum { requests errors subrequests }
      quantiles { cpuTimeP50 cpuTimeP99 }
    }
  } }
}`
	body := map[string]any{"query": q, "variables": map[string]any{
		"acct": acct, "script": script,
		"since": now.Add(-24 * time.Hour).UTC().Format(time.RFC3339), "until": now.UTC().Format(time.RFC3339)}}
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cloudflareAPI+"/graphql", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			Viewer struct {
				Accounts []struct {
					Rows []struct {
						Sum struct {
							Requests    int64 `json:"requests"`
							Errors      int64 `json:"errors"`
							Subrequests int64 `json:"subrequests"`
						} `json:"sum"`
						Quantiles struct {
							P50 float64 `json:"cpuTimeP50"`
							P99 float64 `json:"cpuTimeP99"`
						} `json:"quantiles"`
					} `json:"workersInvocationsAdaptive"`
				} `json:"accounts"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("analytics answered with status %s", resp.Status)
	}
	if len(out.Errors) > 0 {
		return nil, errors.New(out.Errors[0].Message)
	}
	st := &cfWorkerStats{}
	for _, a := range out.Data.Viewer.Accounts {
		for _, r := range a.Rows {
			st.Requests += r.Sum.Requests
			st.Errors += r.Sum.Errors
			st.Subrequests += r.Sum.Subrequests
			st.CPUP50, st.CPUP99 = r.Quantiles.P50, r.Quantiles.P99
		}
	}
	return st, nil
}

func jsonReader(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
