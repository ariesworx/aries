package doltmetrics

import (
	"os"
	"strings"
	"testing"
)

// The guide, the Docker test server and the refusals name one endpoint:
// Dolt's config turns it on where starfixd's config says to read it, and
// where the refusal of a bad URL suggests.
func TestGuideAndImageNameOneEndpoint(t *testing.T) {
	if err := CheckURL(Example); err != nil {
		t.Fatalf("CheckURL(Example) = %v: the refusals suggest a URL that is refused", err)
	}
	// The metrics: block of a Dolt config, as both write it.
	const block = "metrics:\n  host: 127.0.0.1\n  port: 11228\n  labels: {}\n"
	if want := "http://127.0.0.1:11228/metrics"; Example != want {
		t.Fatalf("Example = %q; the block below, and this test, are written for %q", Example, want)
	}
	for _, tc := range []struct {
		file string
		want []string
	}{
		{"../../docs/server.md", []string{block, "dolt_metrics_url: " + Example, "STARFIXD_TELEMETRY_DOLT_METRICS_URL",
			`printf 'metrics:\n  host: 127.0.0.1\n  port: 11228\n  labels: {}\n' >> /etc/dolt/config.yaml`}},
		{"../../deploy/docker/dolt.yaml", []string{block}},
		{"../../deploy/docker/entrypoint.sh", []string{"dolt_metrics_url=" + Example + "\n"}},
		{"../../deploy/docker/smoke.sh", []string{"/dev/tcp/127.0.0.1/11228"}},
		{"../../docs/deploy/docker.md", []string{Example}},
		{"../../docs/security-model.md", []string{"dolt_metrics_url"}},
	} {
		b, err := os.ReadFile(tc.file)
		if err != nil {
			t.Errorf("%s: %v", tc.file, err)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s lacks %q: Dolt's metrics endpoint is %s everywhere, so change them together", tc.file, want, Example)
			}
		}
	}
}
