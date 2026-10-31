package server

import (
	"github.com/Maffewem/Orchestrator/internal/store"
	"reflect"
	"testing"
)

func TestTidyEnvKeepsCommentsAndBlankLines(t *testing.T) {
	got := tidyEnv([]string{"", "# db", "A=1", "  ", "", "# api (secret)", " B=2 ", "A=1", ""})
	want := []string{"# db", "A=1", "", "# api (secret)", "B=2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSpecDropsEnvComments(t *testing.T) {
	p := &store.Project{Name: "web", Source: store.Source{Type: store.SourceImage, Image: "nginx"},
		Runtime: store.Runtime{Env: []string{"# note", "A=1", "", "B="}}}
	spec, err := specFor(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A=1", "B="}; !reflect.DeepEqual(spec.Env, want) {
		t.Fatalf("got %q want %q", spec.Env, want)
	}
}
