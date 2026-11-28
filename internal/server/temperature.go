package server

import (
	"context"
	"net/http"
	"strconv"
)

// Device temperature alert thresholds in °C. A device raises a warning at the first and a
// critical issue at the second; both feed Needs attention and the alerts bell.
const (
	settingTempWarn = "temp_warn_c"
	settingTempCrit = "temp_crit_c"
	defaultTempWarn = 80.0
	defaultTempCrit = 85.0
)

func (s *Server) tempThresholds(ctx context.Context) (warn, crit float64) {
	warn, crit = defaultTempWarn, defaultTempCrit
	if s.store == nil {
		return
	}
	if v, err := s.store.GetSetting(ctx, settingTempWarn); err == nil {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			warn = f
		}
	}
	if v, err := s.store.GetSetting(ctx, settingTempCrit); err == nil {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			crit = f
		}
	}
	return
}

type tempView struct {
	Warn float64 `json:"warn_c"`
	Crit float64 `json:"crit_c"`
}

func (s *Server) tempView(ctx context.Context) tempView {
	w, c := s.tempThresholds(ctx)
	return tempView{Warn: w, Crit: c}
}

func (s *Server) handleSetTemperature(w http.ResponseWriter, r *http.Request) {
	var req tempView
	if !decode(w, r, &req) {
		return
	}
	if req.Warn < 30 || req.Crit > 150 || req.Warn >= req.Crit {
		writeError(w, http.StatusBadRequest, "warning must be at least 30°C, critical at most 150°C, and warning below critical")
		return
	}
	ctx := r.Context()
	for k, v := range map[string]float64{settingTempWarn: req.Warn, settingTempCrit: req.Crit} {
		if err := s.store.SetSetting(ctx, k, strconv.FormatFloat(v, 'f', -1, 64)); err != nil {
			s.internal(w, err)
			return
		}
	}
	s.event(ctx, "", "info", "settings.temperature", "Temperature thresholds updated")
	writeJSON(w, http.StatusOK, s.tempView(ctx))
}
