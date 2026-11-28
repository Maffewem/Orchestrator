package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Docker housekeeping. Worker images built here are tagged orchestrator/<worker>:<sha>. Nothing
// else removes them, so this trims old ones: newest first are kept, by count and/or by a size
// budget; images used by any container, and anything younger than gcMinAge (it may belong to a
// deployment still in flight), are never touched.

const (
	gcMinAge     = time.Hour
	gcRepoPrefix = "orchestrator/"
)

type gcImage struct {
	ID      string
	Tags    []string
	Created int64
	Size    int64
	InUse   bool
}

type gcCandidate struct {
	Tag, Repo, ID string
	Created, Size int64
}

// planTrim returns the worker image tags to remove. keep (0 = no limit) is the number of newest
// versions kept per worker; maxBytes (0 = no limit) is a budget for the images that stay, so the
// oldest unprotected ones go first until it fits. liveWorkers are the containers present; repos
// of workers that are gone keep nothing but protected images. only limits the plan to one repo.
func planTrim(imgs []gcImage, keep int, maxBytes int64, now time.Time, liveWorkers map[string]bool, only string) []gcCandidate {
	groups := map[string][]gcCandidate{}
	protected := map[string]bool{} // tag -> must stay
	for _, im := range imgs {
		for _, t := range im.Tags {
			repo, _ := splitImage(t)
			if !strings.HasPrefix(repo, gcRepoPrefix) || (only != "" && repo != only) {
				continue
			}
			groups[repo] = append(groups[repo], gcCandidate{Tag: t, Repo: repo, ID: im.ID, Created: im.Created, Size: im.Size})
			if im.InUse || now.Sub(time.Unix(im.Created, 0)) < gcMinAge {
				protected[t] = true
			}
		}
	}
	var remove []gcCandidate
	var all []gcCandidate // what is left after the count rule, for the size budget
	for repo, g := range groups {
		sort.Slice(g, func(i, j int) bool { return g[i].Created > g[j].Created })
		orphan := !liveWorkers[strings.TrimPrefix(repo, gcRepoPrefix)]
		for i, c := range g {
			over := keep > 0 && i >= keep
			if orphan && only == "" {
				over = true
			}
			if over && !protected[c.Tag] {
				remove = append(remove, c)
			} else {
				all = append(all, c)
			}
		}
	}
	if maxBytes > 0 {
		var total int64
		seen := map[string]bool{}
		for _, c := range all {
			if !seen[c.ID] {
				seen[c.ID] = true
				total += c.Size
			}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Created < all[j].Created }) // oldest first
		// The newest version of every worker is always kept, whatever the budget says.
		newest := map[string]string{}
		for _, c := range all {
			if cur, ok := newest[c.Repo]; !ok || c.Created > findCreated(all, cur) {
				newest[c.Repo] = c.Tag
			}
		}
		for _, c := range all {
			if total <= maxBytes {
				break
			}
			if protected[c.Tag] || newest[c.Repo] == c.Tag {
				continue
			}
			remove = append(remove, c)
			total -= c.Size
		}
	}
	return remove
}

func findCreated(cs []gcCandidate, tag string) int64 {
	for _, c := range cs {
		if c.Tag == tag {
			return c.Created
		}
	}
	return 0
}

type dfImage struct {
	ID         string   `json:"Id"`
	RepoTags   []string `json:"RepoTags"`
	Created    int64    `json:"Created"`
	Size       int64    `json:"Size"`
	Containers int64    `json:"Containers"`
}

type dfResult struct {
	LayersSize int64     `json:"LayersSize"`
	Images     []dfImage `json:"Images"`
	BuildCache []struct {
		InUse bool  `json:"InUse"`
		Size  int64 `json:"Size"`
	} `json:"BuildCache"`
}

func isDangling(im dfImage) bool {
	for _, t := range im.RepoTags {
		if t != "" && t != "<none>:<none>" {
			return false
		}
	}
	return true
}

// gcSnapshot reads images, containers and build cache from the engine.
func (d *Docker) gcSnapshot(ctx context.Context) (dfResult, []gcImage, map[string]bool, error) {
	var df dfResult
	q := url.Values{"type": {"image", "build-cache"}}
	if err := d.call(ctx, http.MethodGet, "/system/df", q, nil, &df); err != nil {
		if err := d.call(ctx, http.MethodGet, "/system/df", nil, nil, &df); err != nil { // older engines ignore the filter
			return df, nil, nil, err
		}
	}
	cs, err := d.listContainers(ctx, nil)
	if err != nil {
		return df, nil, nil, err
	}
	used := map[string]bool{}
	live := map[string]bool{}
	for _, c := range cs {
		used[c.ImageID] = true
		for _, n := range c.Names {
			live[strings.TrimPrefix(n, "/")] = true
		}
	}
	var imgs []gcImage
	for _, im := range df.Images {
		g := gcImage{ID: im.ID, Created: im.Created, Size: im.Size, InUse: used[im.ID] || im.Containers > 0}
		for _, t := range im.RepoTags {
			if t != "<none>:<none>" {
				g.Tags = append(g.Tags, t)
			}
		}
		imgs = append(imgs, g)
	}
	return df, imgs, live, nil
}

// DockerReport describes the images and cache on the device and what a prune would remove.
func (d *Docker) DockerReport(ctx context.Context, keep int, maxBytes int64) (*api.DockerStorage, error) {
	df, imgs, live, err := d.gcSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	rep := &api.DockerStorage{Keep: keep, LayersSize: df.LayersSize, Workers: []api.DockerImageGroup{}}
	if raw, err := d.infoRaw(ctx); err == nil {
		rep.RootDir = raw
	}
	now := time.Now()
	removable := map[string]bool{}
	for _, c := range planTrim(imgs, keep, maxBytes, now, live, "") {
		removable[c.Tag] = true
	}
	groups := map[string]*api.DockerImageGroup{}
	for _, im := range df.Images {
		rep.Images++
		rep.ImagesSize += im.Size
		if isDangling(im) {
			rep.Dangling++
			rep.DanglingSize += im.Size
			rep.Reclaimable += im.Size
			continue
		}
		if im.Containers <= 0 {
			rep.Unused++
			rep.UnusedSize += im.Size
		}
	}
	for _, im := range imgs {
		for _, t := range im.Tags {
			repo, _ := splitImage(t)
			if !strings.HasPrefix(repo, gcRepoPrefix) {
				continue
			}
			g := groups[repo]
			if g == nil {
				w := strings.TrimPrefix(repo, gcRepoPrefix)
				g = &api.DockerImageGroup{Repo: repo, Worker: w, Orphaned: !live[w]}
				groups[repo] = g
			}
			g.Versions = append(g.Versions, api.DockerImageVersion{Tag: t, ID: im.ID, Size: im.Size, Created: im.Created, InUse: im.InUse, Removable: removable[t]})
			if removable[t] {
				rep.Reclaimable += im.Size
			}
		}
	}
	for _, g := range groups {
		sort.Slice(g.Versions, func(i, j int) bool { return g.Versions[i].Created > g.Versions[j].Created })
		rep.Workers = append(rep.Workers, *g)
	}
	sort.Slice(rep.Workers, func(i, j int) bool { return rep.Workers[i].Worker < rep.Workers[j].Worker })
	for _, b := range df.BuildCache {
		rep.BuildCache += b.Size
		if !b.InUse {
			rep.BuildCacheFree += b.Size
		}
	}
	rep.Reclaimable += rep.BuildCacheFree
	return rep, nil
}

func (d *Docker) infoRaw(ctx context.Context) (string, error) {
	var v struct {
		DockerRootDir string `json:"DockerRootDir"`
	}
	err := d.call(ctx, http.MethodGet, "/info", nil, nil, &v)
	return v.DockerRootDir, err
}

// PruneOptions chooses what a prune removes.
type PruneOptions struct {
	Keep       int   // newest versions of each worker image to keep (0 = no count limit)
	MaxBytes   int64 // size budget for worker images (0 = none)
	Trim       bool  // apply Keep/MaxBytes to worker images
	Dangling   bool
	Unused     bool // every image no container uses, built or pulled
	BuildCache bool
	CacheHours int // only cache older than this (0 = all of it)
	Only       string
}

// Prune removes what opts selects and reports the space the engine gave back.
func (d *Docker) Prune(ctx context.Context, opts PruneOptions) (*api.DockerPruneResult, error) {
	before, imgs, live, err := d.gcSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	res := &api.DockerPruneResult{Steps: []string{}}
	if opts.Trim {
		n := 0
		for _, c := range planTrim(imgs, opts.Keep, opts.MaxBytes, time.Now(), live, opts.Only) {
			// Not forced: the engine refuses an image a container still uses.
			if err := d.call(ctx, http.MethodDelete, "/images/"+url.PathEscape(c.Tag), nil, nil, nil); err != nil {
				res.Steps = append(res.Steps, fmt.Sprintf("kept %s: %v", c.Tag, err))
				continue
			}
			n++
		}
		res.ImagesRemoved += n
		res.Steps = append(res.Steps, fmt.Sprintf("removed %d old worker image(s)", n))
	}
	if opts.Dangling || opts.Unused {
		f := `{"dangling":["true"],"until":["1h"]}`
		what := "untagged"
		if opts.Unused {
			f, what = `{"dangling":["false"],"until":["1h"]}`, "unused"
		}
		var out struct {
			ImagesDeleted []json.RawMessage `json:"ImagesDeleted"`
		}
		if err := d.call(ctx, http.MethodPost, "/images/prune", url.Values{"filters": {f}}, nil, &out); err != nil {
			return res, fmt.Errorf("prune %s images: %w", what, err)
		}
		res.ImagesRemoved += len(out.ImagesDeleted)
		res.Steps = append(res.Steps, fmt.Sprintf("pruned %s images (%d layers/tags removed)", what, len(out.ImagesDeleted)))
	}
	if opts.BuildCache {
		q := url.Values{}
		if opts.CacheHours > 0 {
			q.Set("filters", fmt.Sprintf(`{"until":["%dh"]}`, opts.CacheHours))
		} else {
			q.Set("all", "true")
		}
		var out struct {
			SpaceReclaimed int64 `json:"SpaceReclaimed"`
		}
		if err := d.call(ctx, http.MethodPost, "/build/prune", q, nil, &out); err != nil {
			return res, fmt.Errorf("prune build cache: %w", err)
		}
		res.Steps = append(res.Steps, "pruned build cache ("+strconv.FormatInt(out.SpaceReclaimed, 10)+" bytes)")
	}
	if after, _, _, err := d.gcSnapshot(ctx); err == nil {
		bc := func(r dfResult) (n int64) {
			for _, b := range r.BuildCache {
				n += b.Size
			}
			return
		}
		res.Reclaimed = before.LayersSize - after.LayersSize + bc(before) - bc(after)
		if res.Reclaimed < 0 {
			res.Reclaimed = 0
		}
	}
	return res, nil
}

// dockerGC runs docker.report and docker.prune commands.
func (a *Agent) dockerGC(ctx context.Context, c api.Command) (string, error) {
	keep, _ := strconv.Atoi(c.Args["keep"])
	maxBytes, _ := strconv.ParseInt(c.Args["max_bytes"], 10, 64)
	if keep < 0 || maxBytes < 0 {
		return "", fmt.Errorf("keep and max_bytes cannot be negative")
	}
	switch c.Action {
	case api.ActionDockerReport:
		rep, err := a.docker.DockerReport(ctx, keep, maxBytes)
		if err != nil {
			return "", err
		}
		b, err := json.Marshal(rep)
		return string(b), err
	case api.ActionDockerPrune:
		hours, _ := strconv.Atoi(c.Args["cache_hours"])
		res, err := a.docker.Prune(ctx, PruneOptions{
			Keep: keep, MaxBytes: maxBytes, Trim: c.Args["trim"] == "1", Dangling: c.Args["dangling"] == "1",
			Unused: c.Args["unused"] == "1", BuildCache: c.Args["build_cache"] == "1", CacheHours: hours,
		})
		if res == nil {
			return "", err
		}
		b, _ := json.Marshal(res)
		return string(b), err
	}
	return "", fmt.Errorf("unsupported action %q", c.Action)
}
