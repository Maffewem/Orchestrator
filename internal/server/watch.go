package server

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/store"
)

// globRe turns a watch path such as "docs/**", "*.md" or "src/" into a matcher. "**" crosses
// directories, "*" does not, a trailing slash means everything below, and a lone "*" is everything.
func globRe(pat string) *regexp.Regexp {
	pat = strings.TrimPrefix(strings.TrimSpace(pat), "/")
	if pat == "*" || pat == "" {
		return regexp.MustCompile(`.*`)
	}
	if strings.HasSuffix(pat, "/") {
		pat += "**"
	}
	var b strings.Builder
	for i := 0; i < len(pat); i++ {
		switch {
		case strings.HasPrefix(pat[i:], "**"):
			b.WriteString(`.*`)
			i++
		case pat[i] == '*':
			b.WriteString(`[^/]*`)
		default:
			b.WriteString(regexp.QuoteMeta(string(pat[i])))
		}
	}
	// a plain name also matches everything below it, so "docs" covers "docs/a.md"
	return regexp.MustCompile(`^` + b.String() + `(/.*)?$`)
}

func matchesAny(globs []string, file string) bool {
	for _, g := range globs {
		if globRe(g).MatchString(file) {
			return true
		}
	}
	return false
}

// wants reports whether a change to files should build. Unknown files (nil) always build,
// so a watch rule can only ever skip a build it can prove is irrelevant.
func wants(w store.Watch, files []string) bool {
	if files == nil || (len(w.Include) == 0 && len(w.Exclude) == 0) {
		return true
	}
	for _, f := range files {
		f = strings.TrimPrefix(f, "/")
		if (len(w.Include) == 0 || matchesAny(w.Include, f)) && !matchesAny(w.Exclude, f) {
			return true
		}
	}
	return false
}

// cleanWatch trims globs and drops empty ones.
func cleanWatch(w *store.Watch) error {
	for _, l := range []*[]string{&w.Include, &w.Exclude} {
		out := []string{}
		for _, g := range *l {
			if g = strings.TrimSpace(g); g == "" {
				continue
			}
			if strings.Contains(g, "..") {
				return fmt.Errorf("watch path %q must stay inside the repository", g)
			}
			out = append(out, g)
		}
		if len(out) == 0 {
			out = nil
		}
		*l = out
	}
	return nil
}

// compareFiles lists the files changed between two commits, or nil when GitHub can't say.
func (s *Server) compareFiles(ctx context.Context, repo, base, head string) []string {
	var out struct {
		Files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	if err := s.githubGet(ctx, fmt.Sprintf("/repos/%s/compare/%s...%s", repo, base, head), &out); err != nil || len(out.Files) == 0 || len(out.Files) >= 300 {
		return nil
	}
	files := []string{}
	for _, f := range out.Files {
		files = append(files, f.Filename)
		if f.PreviousFilename != "" {
			files = append(files, f.PreviousFilename)
		}
	}
	return files
}

// pullFiles lists the files a pull request changes, or nil when GitHub can't say.
func (s *Server) pullFiles(ctx context.Context, repo string, n int) []string {
	var out []struct {
		Filename string `json:"filename"`
	}
	if err := s.githubGet(ctx, fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100", repo, n), &out); err != nil || len(out) == 0 || len(out) >= 100 {
		return nil
	}
	files := []string{}
	for _, f := range out {
		files = append(files, f.Filename)
	}
	return files
}
