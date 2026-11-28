package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSensorsReadsThermalZonesAndHwmon(t *testing.T) {
	root := t.TempDir()
	w := func(p, v string) {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(v+"\n"), 0o644)
	}
	w("sys/class/thermal/thermal_zone0/temp", "52300")
	w("sys/class/thermal/thermal_zone0/type", "cpu-thermal")
	w("sys/class/thermal/thermal_zone1/temp", "0") // ignored
	w("sys/class/hwmon/hwmon0/name", "nvme")
	w("sys/class/hwmon/hwmon0/temp1_input", "41000")
	w("sys/class/hwmon/hwmon0/temp1_label", "Composite")
	a := &Agent{cfg: Config{HostRoot: root}}
	got := a.sensors()
	if len(got) != 2 || got[0].Name != "cpu-thermal" || got[0].C != 52.3 || got[1].Name != "nvme Composite" || got[1].C != 41 {
		t.Fatalf("sensors = %+v", got)
	}
}
