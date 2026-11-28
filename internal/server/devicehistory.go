package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Device resource history. Live samples (one per heartbeat) stay in memory for the last hour. Every
// sample is also folded into three tiers that are kept in SQLite, so graphs survive a restart and
// reach back further:
//
//	1 minute   kept for 24 hours
//	5 minutes  kept for 7 days
//	1 hour     kept for the retention setting (up to 90 days)
//
// Each coarser tier is the average of the tier below it. Counters that only mean something at their
// peak (kernel OOM kills, the power flags) keep the highest value instead of the average.
const settingDeviceMetrics = "device_metrics"

type histTier struct {
	res  int           // seconds per point
	keep time.Duration // longest age kept, before the retention setting caps it
}

var histTiers = []histTier{{60, 24 * time.Hour}, {300, 7 * 24 * time.Hour}, {3600, 90 * 24 * time.Hour}}

var deviceRetentions = []int{1, 7, 30, 90}

type deviceMetricsSettings struct {
	RetentionDays int `json:"retention_days"`
}

func (s *Server) deviceRetention(ctx context.Context) int {
	st := deviceMetricsSettings{RetentionDays: 30}
	if s.store != nil {
		s.getJSON(ctx, settingDeviceMetrics, &st)
	}
	if !slices.Contains(deviceRetentions, st.RetentionDays) {
		return 30
	}
	return st.RetentionDays
}

// tierKeep is how long a tier keeps its points under a retention setting.
func tierKeep(t histTier, days int) time.Duration {
	return min(t.keep, time.Duration(days)*24*time.Hour)
}

// histTiers fold. buf holds the open bucket of each tier for each device.
type deviceHistory struct {
	mu   sync.Mutex
	open map[string]*[3]openBucket // device ID
}

type openBucket struct {
	start int64
	buf   []MetricSample
}

func newDeviceHistory() *deviceHistory { return &deviceHistory{open: map[string]*[3]openBucket{}} }

// record folds one live sample into the tiers and stores every bucket that has just closed.
func (s *Server) recordHistory(ctx context.Context, deviceID string, smp MetricSample) {
	if s.store == nil || s.hist == nil {
		return
	}
	ctx = context.WithoutCancel(ctx) // a closed request must not lose a finished bucket
	s.hist.mu.Lock()
	var out []func()
	b := s.hist.open[deviceID]
	if b == nil {
		b = &[3]openBucket{}
		s.hist.open[deviceID] = b
	}
	cur := smp
	for i, t := range histTiers {
		start := cur.Time.Unix() / int64(t.res) * int64(t.res)
		ob := &b[i]
		if ob.start != start && len(ob.buf) > 0 {
			avg := averageSamples(ob.buf)
			avg.Time = time.Unix(ob.start, 0).UTC()
			ob.buf = ob.buf[:0]
			ob.start = start
			ob.buf = append(ob.buf, cur)
			res, ts := t.res, avg.Time.Unix()
			if data, err := json.Marshal(avg); err == nil {
				out = append(out, func() {
					if err := s.store.PutDeviceSample(ctx, deviceID, res, ts, data); err != nil {
						s.log.Warn("store device sample", "err", err)
					}
				})
			}
			cur = avg // the next tier is built from this tier's average
			continue
		}
		ob.start = start
		ob.buf = append(ob.buf, cur)
		break
	}
	s.hist.mu.Unlock()
	for _, f := range out {
		f()
	}
}

// averageSamples reduces one bucket to a single sample.
func averageSamples(in []MetricSample) MetricSample {
	n := float64(len(in))
	var o MetricSample
	var swapUsed float64
	var clock, maxClock float64
	var nClock int
	disks := map[string]*api.DiskIO{}
	nets := map[string]*api.NetIO{}
	counts := map[string]float64{}
	var diskOrder, netOrder []string
	for _, x := range in {
		o.CPUPercent += x.CPUPercent
		o.MemPercent += x.MemPercent
		o.DiskPerc += x.DiskPerc
		o.Load1 += x.Load1
		o.Temp = math.Max(o.Temp, x.Temp)
		o.IOWait += x.IOWait
		o.Swap += x.Swap
		swapUsed += float64(x.SwapUsed)
		o.OOM = max(o.OOM, x.OOM)
		if x.Clock > 0 {
			clock += x.Clock
			nClock++
		}
		maxClock = math.Max(maxClock, x.MaxClock)
		if x.HasPower {
			o.HasPower = true
			o.Power |= x.Power
		}
		for _, d := range x.Disks {
			a := disks[d.Name]
			if a == nil {
				a = &api.DiskIO{Name: d.Name}
				disks[d.Name] = a
				diskOrder = append(diskOrder, d.Name)
			}
			a.ReadBps += d.ReadBps
			a.WriteBps += d.WriteBps
			a.ReadIOPS += d.ReadIOPS
			a.WriteIOPS += d.WriteIOPS
			a.Busy += d.Busy
			counts["d"+d.Name]++
		}
		for _, d := range x.Nets {
			a := nets[d.Name]
			if a == nil {
				a = &api.NetIO{Name: d.Name}
				nets[d.Name] = a
				netOrder = append(netOrder, d.Name)
			}
			a.RxBps += d.RxBps
			a.TxBps += d.TxBps
			a.Errors += d.Errors
			a.Wireless = a.Wireless || d.Wireless
			a.SignalDBm += d.SignalDBm
			counts["n"+d.Name]++
		}
	}
	o.CPUPercent /= n
	o.MemPercent /= n
	o.DiskPerc /= n
	o.Load1 /= n
	o.IOWait /= n
	o.Swap /= n
	o.SwapUsed = uint64(swapUsed / n)
	if nClock > 0 {
		o.Clock = clock / float64(nClock)
	}
	o.MaxClock = maxClock
	o.Time = in[len(in)-1].Time
	for _, name := range diskOrder {
		a, c := disks[name], counts["d"+name]
		a.ReadBps, a.WriteBps, a.ReadIOPS, a.WriteIOPS, a.Busy = a.ReadBps/c, a.WriteBps/c, a.ReadIOPS/c, a.WriteIOPS/c, a.Busy/c
		o.Disks = append(o.Disks, *a)
	}
	for _, name := range netOrder {
		a, c := nets[name], counts["n"+name]
		a.RxBps, a.TxBps, a.Errors = a.RxBps/c, a.TxBps/c, a.Errors/c
		if a.Wireless {
			a.SignalDBm = int(math.Round(float64(a.SignalDBm) / c))
		}
		o.Nets = append(o.Nets, *a)
	}
	return o
}

// rangeTier maps a graph range to the tier that serves it. The empty range is the live hour.
func rangeTier(r string) (res int, span time.Duration, ok bool) {
	switch r {
	case "", "1h":
		return 0, time.Hour, true
	case "6h":
		return 60, 6 * time.Hour, true
	case "24h":
		return 60, 24 * time.Hour, true
	case "7d":
		return 300, 7 * 24 * time.Hour, true
	case "30d":
		return 3600, 30 * 24 * time.Hour, true
	case "90d":
		return 3600, 90 * 24 * time.Hour, true
	}
	return 0, 0, false
}

// storedHistory reads a device's stored samples at one resolution.
func (s *Server) storedHistory(ctx context.Context, deviceID string, res int, span time.Duration) ([]MetricSample, error) {
	if s.store == nil {
		return []MetricSample{}, nil
	}
	rows, err := s.store.DeviceSamples(ctx, deviceID, res, time.Now().Add(-span).Unix())
	if err != nil {
		return nil, err
	}
	out := make([]MetricSample, 0, len(rows))
	for _, r := range rows {
		var m MetricSample
		if json.Unmarshal(r, &m) == nil {
			out = append(out, m)
		}
	}
	return out, nil
}

// primeMetrics refills the live hour from the stored one-minute samples after a restart.
func (s *Server) primeMetrics(ctx context.Context) {
	if s.store == nil || s.metrics == nil {
		return
	}
	devs, err := s.store.ListDevices(ctx)
	if err != nil {
		return
	}
	for _, d := range devs {
		if smp, err := s.storedHistory(ctx, d.ID, 60, time.Hour); err == nil {
			s.metrics.prime(d.ID, smp)
		}
	}
}

func (h *metricsHistory) prime(deviceID string, older []MetricSample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.data[deviceID]) > 0 || len(older) == 0 {
		return
	}
	h.data[deviceID] = append([]MetricSample{}, older...)
}

// pruneDeviceHistory removes samples older than each tier keeps them.
func (s *Server) pruneDeviceHistory(ctx context.Context) error {
	days := s.deviceRetention(ctx)
	for _, t := range histTiers {
		if err := s.store.PruneDeviceSamples(ctx, t.res, time.Now().Add(-tierKeep(t, days)).Unix()); err != nil {
			return err
		}
	}
	return nil
}

type deviceMetricsView struct {
	RetentionDays int     `json:"retention_days"`
	Choices       []int   `json:"choices"`
	Rows          int64   `json:"rows"`
	Bytes         int64   `json:"bytes"`
	PerDevice     []int64 `json:"per_device_bytes"` // estimated bytes per device at each choice
}

// deviceSampleBytes is a rough size of one stored sample, with its disks and interfaces.
const deviceSampleBytes = 420

func deviceHistoryRows(days int) int64 {
	var n int64
	for _, t := range histTiers {
		n += int64(tierKeep(t, days) / (time.Duration(t.res) * time.Second))
	}
	return n
}

func (s *Server) deviceMetricsView(ctx context.Context) deviceMetricsView {
	v := deviceMetricsView{RetentionDays: s.deviceRetention(ctx), Choices: deviceRetentions}
	for _, d := range deviceRetentions {
		v.PerDevice = append(v.PerDevice, deviceHistoryRows(d)*deviceSampleBytes)
	}
	if s.store != nil {
		v.Rows, v.Bytes, _ = s.store.DeviceSampleStats(ctx)
	}
	return v
}

func (s *Server) handleGetDeviceMetricsSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.deviceMetricsView(r.Context()))
}

func (s *Server) handleSetDeviceMetricsSettings(w http.ResponseWriter, r *http.Request) {
	var req deviceMetricsSettings
	if !decode(w, r, &req) {
		return
	}
	if !slices.Contains(deviceRetentions, req.RetentionDays) {
		writeError(w, http.StatusBadRequest, "retention must be 1, 7, 30 or 90 days")
		return
	}
	s.setJSON(r.Context(), settingDeviceMetrics, req)
	s.event(r.Context(), "", "info", "settings.device-metrics", fmt.Sprintf("Device graph history is now kept for %d days", req.RetentionDays))
	if err := s.pruneDeviceHistory(r.Context()); err != nil {
		s.log.Warn("prune device history", "err", err)
	}
	writeJSON(w, http.StatusOK, s.deviceMetricsView(r.Context()))
}
