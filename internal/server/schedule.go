package server

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // zone names work even in minimal images
)

// Schedule says when the control plane checks for and installs updates by itself.
type Schedule struct {
	Mode    string `json:"mode"`    // off | daily | weekly
	Time    string `json:"time"`    // HH:MM in TZ
	Weekday int    `json:"weekday"` // 0 = Sunday, for weekly
	TZ      string `json:"tz"`      // IANA name
	Agents  bool   `json:"agents"`  // also update device agents
}

func defaultSchedule() Schedule { return Schedule{Mode: "off", Time: "03:00", Weekday: 0, TZ: "UTC"} }

func (s Schedule) validate() error {
	switch s.Mode {
	case "off", "daily", "weekly":
	default:
		return errors.New("mode must be off, daily or weekly")
	}
	if _, _, err := s.clock(); err != nil {
		return err
	}
	if s.Weekday < 0 || s.Weekday > 6 {
		return errors.New("weekday must be 0 (Sunday) to 6 (Saturday)")
	}
	if _, err := time.LoadLocation(s.TZ); err != nil {
		return fmt.Errorf("unknown time zone %q", s.TZ)
	}
	return nil
}

func (s Schedule) clock() (h, m int, err error) {
	hh, mm, ok := strings.Cut(s.Time, ":")
	h, e1 := strconv.Atoi(hh)
	m, e2 := strconv.Atoi(mm)
	if !ok || e1 != nil || e2 != nil || len(hh) != 2 || len(mm) != 2 || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, errors.New("time must be HH:MM")
	}
	return h, m, nil
}

func (s Schedule) loc() *time.Location {
	if l, err := time.LoadLocation(s.TZ); err == nil {
		return l
	}
	return time.UTC
}

// lastSlot is the most recent scheduled moment at or before now.
func (s Schedule) lastSlot(now time.Time) (time.Time, bool) {
	if s.Mode == "off" {
		return time.Time{}, false
	}
	h, m, err := s.clock()
	if err != nil {
		return time.Time{}, false
	}
	n := now.In(s.loc())
	back := 0
	if s.Mode == "weekly" {
		back = (int(n.Weekday()) - s.Weekday + 7) % 7
	}
	slot := time.Date(n.Year(), n.Month(), n.Day()-back, h, m, 0, 0, n.Location())
	if slot.After(n) {
		step := 1
		if s.Mode == "weekly" {
			step = 7
		}
		slot = time.Date(slot.Year(), slot.Month(), slot.Day()-step, h, m, 0, 0, n.Location())
	}
	return slot, true
}

// nextSlot is the first scheduled moment after t.
func (s Schedule) nextSlot(t time.Time) (time.Time, bool) {
	last, ok := s.lastSlot(t)
	if !ok {
		return time.Time{}, false
	}
	step := 1
	if s.Mode == "weekly" {
		step = 7
	}
	h, m, _ := s.clock()
	return time.Date(last.Year(), last.Month(), last.Day()+step, h, m, 0, 0, last.Location()), true
}

// due reports whether a slot has passed since baseline (when the schedule was set or last ran).
func (s Schedule) due(baseline, now time.Time) bool {
	slot, ok := s.lastSlot(now)
	return ok && slot.After(baseline)
}
