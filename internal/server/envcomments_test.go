package server

import (
	"reflect"
	"testing"
)

func TestDropEnvComments(t *testing.T) {
	got := cleanLabels(dropEnvComments([]string{"# note (a password)", "A=1", "  # indented", "B="}))
	if want := []string{"A=1", "B="}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
