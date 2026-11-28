package agent

import (
	"github.com/Maffewem/Orchestrator/internal/api"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseThrottled(t *testing.T) {
	for in, want := range map[string]uint32{"0x50005": 0x50005, "50005\n": 0x50005, "0": 0, "throttled": 0} {
		got, ok := parseThrottled(in)
		if in == "throttled" {
			if ok {
				t.Errorf("%q parsed", in)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%q = %x %v, want %x", in, got, ok, want)
		}
	}
}

func TestParseDiskstatsAndNet(t *testing.T) {
	ds := "   8 0 sda 100 0 2000 50 40 0 800 20 0 70 90\n   8 1 sda1 90 0 1800 40 30 0 700 10 0 60 80\n 7 0 loop0 1 0 2 0 0 0 0 0 0 0 0\n"
	got := parseDiskstats(ds, func(n string) bool { return n == "sda" || n == "loop0" })
	if len(got) != 1 || got["sda"] != [7]uint64{100, 2000, 40, 800, 70} {
		t.Fatalf("diskstats = %v", got)
	}
	nd := "Inter-|   Receive\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop\n" +
		"    lo: 5 1 0 0 0 0 0 0 5 1 0 0 0 0 0 0\n  eth0: 1000 10 1 2 0 0 0 0 500 5 3 4 0 0 0 0\n veth1: 9 1 0 0 0 0 0 0 9 1 0 0 0 0 0 0\n"
	n := parseNetDev(nd)
	if len(n) != 1 || n["eth0"] != [3]uint64{1000, 500, 10} {
		t.Fatalf("net = %v", n)
	}
	w := parseWireless("Inter-| sta-|   Quality\n face | tus | link level noise\n wlan0: 0000   60.  -52.  -256 0 0 0 0 0 0\n")
	if w["wlan0"] != -52 {
		t.Fatalf("wireless = %v", w)
	}
}

func TestHardwareMetricsRates(t *testing.T) {
	root := t.TempDir()
	w := func(p, v string) {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(v+"\n"), 0o644)
	}
	os.MkdirAll(filepath.Join(root, "sys/block/mmcblk0"), 0o755)
	w("proc/diskstats", "179 0 mmcblk0 100 0 2000 50 40 0 800 20 0 1000 90")
	w("proc/net/dev", "h|h\nh|h\n eth0: 1000 10 0 0 0 0 0 0 500 5 0 0 0 0 0 0\n wlan0: 1 1 0 0 0 0 0 0 1 1 0 0 0 0 0 0")
	w("proc/net/wireless", "a\nb\n wlan0: 0 60. -70. -256 0 0 0 0 0 0")
	w("proc/vmstat", "nr_free_pages 1\noom_kill 3")
	w("proc/stat", "cpu 100 0 100 700 100 0 0 0")
	w("sys/devices/platform/soc/soc:firmware/get_throttled", "50005")
	w("sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq", "1500000")
	a := &Agent{cfg: Config{HostRoot: root}}
	var m1 api.Metrics
	a.hardwareMetrics(&m1)
	if len(m1.Disks) != 0 || m1.OOMKills != 3 || m1.Power == nil || !m1.Power.UnderVoltage || !m1.Power.SinceThrottled || m1.Power.FreqCapped || m1.ClockMHz != 1500 {
		t.Fatalf("first report = %+v power=%+v", m1, m1.Power)
	}
	if len(m1.Nets) != 1 || m1.Nets[0].Name != "wlan0" || !m1.Nets[0].Wireless || m1.Nets[0].SignalDBm != -70 {
		t.Fatalf("first nets = %+v", m1.Nets)
	}
	// Ten seconds later: 500 more sectors read, 100 written, 2.5 s busy, 5000 more bytes received.
	a.hw.at = a.hw.at.Add(-10 * time.Second)
	w("proc/diskstats", "179 0 mmcblk0 150 0 2500 50 60 0 900 20 0 3500 90")
	w("proc/net/dev", "h|h\nh|h\n eth0: 6000 60 5 5 0 0 0 0 1500 15 0 0 0 0 0 0\n wlan0: 1 1 0 0 0 0 0 0 1 1 0 0 0 0 0 0")
	w("proc/stat", "cpu 150 0 150 750 150 0 0 0")
	var m2 api.Metrics
	a.hardwareMetrics(&m2)
	if len(m2.Disks) != 1 {
		t.Fatalf("disks = %+v", m2.Disks)
	}
	d := m2.Disks[0]
	if !near(d.ReadBps, 25600) || !near(d.WriteBps, 5120) || !near(d.ReadIOPS, 5) || !near(d.WriteIOPS, 2) || d.Busy < 24 || d.Busy > 26 {
		t.Fatalf("disk rates = %+v", d)
	}
	var eth *api.NetIO
	for i := range m2.Nets {
		if m2.Nets[i].Name == "eth0" {
			eth = &m2.Nets[i]
		}
	}
	if eth == nil || !near(eth.RxBps, 500) || !near(eth.TxBps, 100) || !near(eth.Errors, 1) {
		t.Fatalf("eth0 = %+v", eth)
	}
	if m2.IOWaitPercent < 24 || m2.IOWaitPercent > 26 {
		t.Fatalf("iowait = %v", m2.IOWaitPercent)
	}
}

func near(got, want float64) bool { return got > want*0.99 && got < want*1.01 }
