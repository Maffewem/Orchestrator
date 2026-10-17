package server

import (
	"testing"
	"time"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestScheduleDaily(t *testing.T) {
	s := Schedule{Mode: "daily", Time: "03:00", TZ: "UTC"}
	if err := s.validate(); err != nil {
		t.Fatal(err)
	}
	set := at(t, "2026-10-17T10:00:00Z")
	if s.due(set, at(t, "2026-10-17T23:00:00Z")) {
		t.Fatal("due before the first slot after being set")
	}
	if !s.due(set, at(t, "2026-10-18T03:00:30Z")) {
		t.Fatal("not due after the slot")
	}
	ran := at(t, "2026-10-18T03:00:30Z")
	if s.due(ran, at(t, "2026-10-18T20:00:00Z")) {
		t.Fatal("due twice for one slot")
	}
	if !s.due(ran, at(t, "2026-10-19T03:01:00Z")) {
		t.Fatal("next day not due")
	}
	next, _ := s.nextSlot(at(t, "2026-10-17T10:00:00Z"))
	if !next.Equal(at(t, "2026-10-18T03:00:00Z")) {
		t.Fatalf("next = %v", next)
	}
}

func TestScheduleWeeklyAndZone(t *testing.T) {
	s := Schedule{Mode: "weekly", Weekday: 1, Time: "04:30", TZ: "Europe/London"} // Monday
	if err := s.validate(); err != nil {
		t.Fatal(err)
	}
	// 2026-10-19 is a Monday; BST ended 2026-10-25, so 04:30 is 03:30 UTC in winter, 03:30 UTC... check both.
	slot, _ := s.lastSlot(at(t, "2026-10-21T12:00:00Z"))
	if !slot.Equal(at(t, "2026-10-19T03:30:00Z")) { // BST (UTC+1)
		t.Fatalf("slot = %v", slot.UTC())
	}
	next, _ := s.nextSlot(at(t, "2026-10-21T12:00:00Z"))
	if !next.Equal(at(t, "2026-10-26T04:30:00Z")) { // GMT after the clocks change
		t.Fatalf("next = %v", next.UTC())
	}
}

func TestScheduleOffAndValidation(t *testing.T) {
	if (Schedule{Mode: "off", Time: "03:00", TZ: "UTC"}).due(time.Time{}, time.Now()) {
		t.Fatal("off schedule is due")
	}
	for _, bad := range []Schedule{
		{Mode: "hourly", Time: "03:00", TZ: "UTC"},
		{Mode: "daily", Time: "3am", TZ: "UTC"},
		{Mode: "daily", Time: "25:00", TZ: "UTC"},
		{Mode: "weekly", Time: "03:00", Weekday: 9, TZ: "UTC"},
		{Mode: "daily", Time: "03:00", TZ: "Mars/Olympus"},
	} {
		if bad.validate() == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}
