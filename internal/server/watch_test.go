package server

import (
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

func TestWants(t *testing.T) {
	cases := []struct {
		name  string
		w     store.Watch
		files []string
		want  bool
	}{
		{"no rules", store.Watch{}, []string{"a.md"}, true},
		{"unknown files", store.Watch{Exclude: []string{"*"}}, nil, true},
		{"star includes all", store.Watch{Include: []string{"*"}}, []string{"src/a.ts"}, true},
		{"exclude docs", store.Watch{Exclude: []string{"docs/**", "*.md"}}, []string{"docs/a/b.png", "README.md"}, false},
		{"one relevant file is enough", store.Watch{Exclude: []string{"docs/"}}, []string{"docs/a.md", "src/a.ts"}, true},
		{"include only src", store.Watch{Include: []string{"src/**"}}, []string{"package.json"}, false},
		{"plain dir name", store.Watch{Include: []string{"web"}}, []string{"web/src/App.tsx"}, true},
		{"star stays in dir", store.Watch{Include: []string{"*.md"}}, []string{"docs/a.md"}, false},
	}
	for _, c := range cases {
		if got := wants(c.w, c.files); got != c.want {
			t.Errorf("%s: wants = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPreviewSourceOverrides(t *testing.T) {
	src := store.Source{Context: "app", Node: &api.NodeSpec{Build: "npm run build", Start: "npm start"}}
	if got := previewSource(src); got.Context != "app" || got.Node.Start != "npm start" {
		t.Fatalf("no overrides should change nothing: %+v", got)
	}
	src.Preview = &store.PreviewBuild{Build: "npm run build:preview", Start: "npm run preview"}
	got := previewSource(src)
	if got.Node.Build != "npm run build:preview" || got.Node.Start != "npm run preview" || got.Context != "app" {
		t.Fatalf("overrides not applied: %+v", got.Node)
	}
	if src.Node.Build != "npm run build" {
		t.Fatal("production settings were modified")
	}
}
