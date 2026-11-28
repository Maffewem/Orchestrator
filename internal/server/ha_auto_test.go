package server

import (
	"testing"
	"time"
)

func TestHAAutoDecide(t *testing.T) {
	now := time.Now()
	base := haState{Role: haRoleStandby, Token: "t", AutoPromote: true, LastSync: now.Add(-2 * time.Minute)}
	cases := []struct {
		name           string
		mod            func(*haState)
		knocked, flt   int
		haveCopy, want bool
	}{
		{"off by default", func(s *haState) { s.AutoPromote = false }, 3, 3, true, false},
		{"still within the wait", func(s *haState) { s.LastSync = now.Add(-10 * time.Second) }, 3, 3, true, false},
		{"no copy yet", func(s *haState) {}, 3, 3, false, false},
		{"small fleet stays manual", func(s *haState) {}, 2, 2, true, false},
		{"link between servers only", func(s *haState) {}, 1, 4, true, false},
		{"exactly half is not a majority", func(s *haState) {}, 2, 4, true, false},
		{"majority lost the active server", func(s *haState) {}, 3, 4, true, true},
		{"all of three", func(s *haState) {}, 3, 3, true, true},
		{"an active server never auto-promotes", func(s *haState) { s.Role = haRoleActive }, 3, 3, true, false},
	}
	for _, c := range cases {
		st := base
		c.mod(&st)
		got, _ := haAutoDecide(st, now, c.knocked, c.flt, c.haveCopy)
		if got != c.want {
			t.Errorf("%s: promote=%v, want %v", c.name, got, c.want)
		}
	}
	if _, note := haAutoDecide(base, now, 1, 2, true); note == "" {
		t.Error("a small fleet should explain why it is not promoting")
	}
}

func TestHAKnocksCountOnlyKnownAgentsInWindow(t *testing.T) {
	s := &Server{}
	s.haAuto.agents = map[string]struct{}{"a": {}, "b": {}, "c": {}}
	s.haAuto.knocks = map[string]time.Time{
		"a": time.Now(),
		"b": time.Now().Add(-5 * time.Minute), // too old
		"z": time.Now(),                       // not in the copy
	}
	k, f := s.haKnockCount(time.Now())
	if k != 1 || f != 3 {
		t.Fatalf("knocked %d of %d", k, f)
	}
}
