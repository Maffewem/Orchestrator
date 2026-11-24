package agent

import "testing"

func TestParseDefaultRoute(t *testing.T) {
	table := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"wlan0\t00000000\t0102A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
		"eth0\t00000000\t0100A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"eth0\t0000A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n"
	iface, gw := parseDefaultRoute(table)
	if iface != "eth0" || gw.String() != "192.168.0.1" {
		t.Fatalf("got %s %s", iface, gw)
	}
	if iface, _ := parseDefaultRoute("Iface\n"); iface != "" {
		t.Fatal("no route expected")
	}
}
