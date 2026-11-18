package agent

import (
	"strings"
	"testing"
)

func TestParseCaddyMetrics(t *testing.T) {
	in := `# HELP caddy_http_request_duration_seconds Histogram of round-trip request durations.
# TYPE caddy_http_request_duration_seconds histogram
caddy_http_request_duration_seconds_bucket{code="200",handler="reverse_proxy",host="app.example.com",method="GET",server="srv0",le="0.01"} 40
caddy_http_request_duration_seconds_bucket{code="200",handler="reverse_proxy",host="app.example.com",method="GET",server="srv0",le="0.1"} 90
caddy_http_request_duration_seconds_bucket{code="200",handler="reverse_proxy",host="app.example.com",method="GET",server="srv0",le="+Inf"} 100
caddy_http_request_duration_seconds_count{code="200",handler="reverse_proxy",host="app.example.com",method="GET",server="srv0"} 100
caddy_http_request_duration_seconds_bucket{code="502",handler="reverse_proxy",host="app.example.com",method="POST",server="srv0",le="0.01"} 0
caddy_http_request_duration_seconds_bucket{code="502",handler="reverse_proxy",host="app.example.com",method="POST",server="srv0",le="0.1"} 3
caddy_http_request_duration_seconds_count{code="502",handler="reverse_proxy",host="app.example.com",method="POST",server="srv0"} 3
caddy_http_request_duration_seconds_count{code="200",handler="metrics",host="127.0.0.1:2019",method="GET",server="srv1"} 7
caddy_http_response_size_bytes_sum{code="200",handler="reverse_proxy",host="app.example.com",method="GET",server="srv0"} 2.5e+06
`
	out := parseCaddyMetrics(strings.NewReader(in))
	if len(out) != 1 {
		t.Fatalf("hosts: %+v", out)
	}
	h := out[0]
	if h.Host != "app.example.com" || h.Codes["2xx"] != 100 || h.Codes["5xx"] != 3 || h.Bytes != 2.5e6 {
		t.Fatalf("host: %+v", h)
	}
	if len(h.Buckets) != 2 || h.Buckets[0].Count != 40 || h.Buckets[1].Count != 93 {
		t.Fatalf("buckets: %+v", h.Buckets)
	}
}
