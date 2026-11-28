package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func tags(cs []gcCandidate) string {
	var t []string
	for _, c := range cs {
		t = append(t, c.Tag)
	}
	sort.Strings(t)
	return strings.Join(t, ",")
}

func TestPlanTrimKeepsNewestAndRunning(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	old := now.Unix() - 86400
	imgs := []gcImage{
		{ID: "a", Tags: []string{"orchestrator/web:a1"}, Created: old - 400, Size: 10},
		{ID: "b", Tags: []string{"orchestrator/web:b2"}, Created: old - 300, Size: 10, InUse: true}, // running, old
		{ID: "c", Tags: []string{"orchestrator/web:c3"}, Created: old - 200, Size: 10},
		{ID: "d", Tags: []string{"orchestrator/web:d4"}, Created: old - 100, Size: 10},
		{ID: "e", Tags: []string{"orchestrator/web:e5"}, Created: now.Unix() - 60, Size: 10}, // building now
		{ID: "x", Tags: []string{"nginx:latest"}, Created: old - 900, Size: 10},
	}
	live := map[string]bool{"web": true}
	// keep 2: newest two are e5 and d4; b2 is running so it stays; a1 and c3 go.
	if got := tags(planTrim(imgs, 2, 0, now, live, "")); got != "orchestrator/web:a1,orchestrator/web:c3" {
		t.Fatalf("keep 2: %s", got)
	}
	// keep 1: only the in-flight image counts as kept; d4, c3, a1 go, running b2 stays.
	if got := tags(planTrim(imgs, 1, 0, now, live, "")); got != "orchestrator/web:a1,orchestrator/web:c3,orchestrator/web:d4" {
		t.Fatalf("keep 1: %s", got)
	}
	// Unrelated images are never planned, and 0 means no count limit.
	if got := planTrim(imgs, 0, 0, now, live, ""); len(got) != 0 {
		t.Fatalf("no limit: %v", got)
	}
}

func TestPlanTrimSizeBudgetAndOrphans(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	old := now.Unix() - 86400
	imgs := []gcImage{
		{ID: "a", Tags: []string{"orchestrator/web:a1"}, Created: old - 300, Size: 100},
		{ID: "b", Tags: []string{"orchestrator/web:b2"}, Created: old - 200, Size: 100},
		{ID: "c", Tags: []string{"orchestrator/web:c3"}, Created: old - 100, Size: 100, InUse: true},
		{ID: "g", Tags: []string{"orchestrator/gone:g1"}, Created: old, Size: 50},
	}
	live := map[string]bool{"web": true}
	// Budget 200: total 350 after dropping nothing by count; orphan removed regardless (keep 0 = no count).
	got := tags(planTrim(imgs, 0, 200, now, live, ""))
	if got != "orchestrator/gone:g1,orchestrator/web:a1" && got != "orchestrator/gone:g1,orchestrator/web:a1,orchestrator/web:b2" {
		t.Fatalf("budget: %s", got)
	}
	// Orphan gone entirely, but the newest of a live worker survives even a tiny budget.
	got = tags(planTrim(imgs, 0, 1, now, live, ""))
	if strings.Contains(got, "c3") || !strings.Contains(got, "a1") || !strings.Contains(got, "b2") {
		t.Fatalf("tiny budget: %s", got)
	}
	// Scoped to one repo, an orphan rule does not apply.
	if got := tags(planTrim(imgs, 1, 0, now, live, "orchestrator/gone")); got != "" {
		t.Fatalf("scoped: %s", got)
	}
}

func fakeEngine(t *testing.T, h http.Handler) *Docker {
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: h}}
	srv.Start()
	t.Cleanup(srv.Close)
	d, err := NewDocker("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPruneAgainstFakeEngine(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	layers := int64(300)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /system/df", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Write([]byte(`{"LayersSize":` + itoa(layers) + `,"Images":[
		 {"Id":"a","RepoTags":["orchestrator/web:a1"],"Created":1000,"Size":100,"Containers":0},
		 {"Id":"b","RepoTags":["orchestrator/web:b2"],"Created":2000,"Size":100,"Containers":1},
		 {"Id":"z","RepoTags":[],"Created":500,"Size":40,"Containers":0}],
		 "BuildCache":[{"InUse":false,"Size":70}]}`))
	})
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"Id":"c1","Names":["/web"],"ImageID":"b"}]`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method == "DELETE" {
			layers = 200
		}
		mu.Unlock()
		w.Write([]byte(`{"ImagesDeleted":[{"Deleted":"z"}],"SpaceReclaimed":70}`))
	})
	d := fakeEngine(t, mux)
	ctx := context.Background()
	rep, err := d.DockerReport(ctx, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Dangling != 1 || rep.DanglingSize != 40 || rep.BuildCacheFree != 70 || len(rep.Workers) != 1 {
		t.Fatalf("report: %+v", rep)
	}
	var removable []string
	for _, v := range rep.Workers[0].Versions {
		if v.Removable {
			removable = append(removable, v.Tag)
		}
	}
	if strings.Join(removable, ",") != "orchestrator/web:a1" || rep.Reclaimable != 100+40+70 {
		t.Fatalf("removable %v reclaimable %d", removable, rep.Reclaimable)
	}
	res, err := d.Prune(ctx, PruneOptions{Keep: 1, Trim: true, Dangling: true, BuildCache: true, CacheHours: 168})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"DELETE /images/orchestrator%2Fweb:a1", "POST /images/prune", "POST /build/prune"}
	joined := strings.Join(calls, "|")
	for _, w := range []string{"DELETE /images/orchestrator/web:a1", "POST /images/prune", "POST /build/prune"} {
		if !strings.Contains(joined, w) {
			t.Fatalf("missing %q (want %v) in %s", w, want, joined)
		}
	}
	if res.Reclaimed != 100 || res.ImagesRemoved != 2 {
		t.Fatalf("result: %+v", res)
	}
}

func itoa(n int64) string {
	return strings.TrimSpace(strings.Repeat(" ", 0) + func() string { return fmtInt(n) }())
}
func fmtInt(n int64) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
