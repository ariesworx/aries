package doltmetrics

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// show prints a sample's figures, each as its value or "-" when the
// body did not give it.
func show(s Sample) string {
	b, err := json.Marshal(s)
	if err != nil {
		return "unprintable: " + err.Error()
	}
	return string(b)
}

func TestParse(t *testing.T) {
	t.Parallel()
	started := time.Unix(1791835308, 0).UTC()
	full := Sample{Version: "2.4.2", Resident: new(uint64(59670528)), MemLimit: new(uint64(1572864000)),
		Started: &started, Conns: new(4), GCCycles: new(int64(3))}
	const body = "dss_dolt_version 3.3816578e+07\n" +
		"dss_concurrent_connections 4\n" +
		"process_resident_memory_bytes 5.9670528e+07\n" +
		"process_start_time_seconds 1.791835308e+09\n" +
		"go_gc_gomemlimit_bytes 1.572864e+09\n" +
		"go_gc_duration_seconds_count 3\n"
	long := strings.Repeat("x", MaxLine)
	tests := []struct {
		name string
		body string
		want Sample
		err  error
	}{
		{name: "every figure", body: body, want: full},
		{name: "comments, blank lines and series it has no use for",
			body: "# HELP dss_dolt_version The version\n# TYPE dss_dolt_version gauge\n\n" + body +
				"go_gc_duration_seconds{quantile=\"0.5\"} 3.7791e-05\ngo_gc_duration_seconds_sum 0.000159914\nsys_cpu_usage 6.75\n",
			want: full},
		{name: "carriage returns and a timestamp", body: strings.ReplaceAll(body, "\n", " 1791835308000\r\n"), want: full},
		{name: "no newline at the end", body: strings.TrimSuffix(body, "\n"), want: full},
		// Dolt's config can add labels of the operator's to its series.
		{name: "labels the operator added", body: strings.ReplaceAll(body, "dss_concurrent_connections 4", `dss_concurrent_connections{env="a b",note="x\"}y{"} 4`), want: full},
		{name: "no memory limit is zero", body: strings.ReplaceAll(body, "1.572864e+09", "9.223372036854776e+18"),
			want: Sample{Version: "2.4.2", Resident: full.Resident, MemLimit: new(uint64(0)), Started: &started, Conns: full.Conns, GCCycles: full.GCCycles}},
		{name: "a missing series is a figure left out", body: "dss_concurrent_connections 4\ngo_gc_duration_seconds_count 3\n",
			want: Sample{Conns: new(4), GCCycles: new(int64(3))}},
		// Two series of one name are two processes, or a forgery: neither
		// is believed.
		{name: "a series given twice is left out", body: body + "process_resident_memory_bytes{instance=\"other\"} 1\n",
			want: Sample{Version: "2.4.2", MemLimit: full.MemLimit, Started: &started, Conns: full.Conns, GCCycles: full.GCCycles}},
		{name: "values that are no count are left out",
			body: "dss_dolt_version -1\ndss_concurrent_connections NaN\nprocess_resident_memory_bytes +Inf\n" +
				"process_start_time_seconds 1e300\ngo_gc_gomemlimit_bytes -5\ngo_gc_duration_seconds_count 3\n",
			want: Sample{GCCycles: new(int64(3))}},
		{name: "a start time no calendar holds is left out", body: "process_start_time_seconds 9e11\ndss_concurrent_connections 4\n", want: Sample{Conns: new(4)}},
		{name: "lines that are not series are passed over",
			body: "dss_concurrent_connections\ndss_concurrent_connections{env=\"a\" 4\nprocess_resident_memory_bytes{ 1\n" +
				"go_gc_gomemlimit_bytes four\n{} 1\n = \ngo_gc_duration_seconds_count 3\n",
			want: Sample{GCCycles: new(int64(3))}},
		{name: "a name that only starts like one", body: "dss_concurrent_connections_total 9\ngo_gc_duration_seconds_count 3\n", want: Sample{GCCycles: new(int64(3))}},
		{name: "a line too long is passed over, and the rest read",
			body: "dss_concurrent_connections{a=\"" + long + "\"} 4\ngo_gc_duration_seconds_count 3\n", want: Sample{GCCycles: new(int64(3))}},
		{name: "a version of three parts", body: "dss_dolt_version 16909060\n", want: Sample{Version: "1.2.772"}},

		{name: "nothing", body: "", err: ErrNoSeries},
		{name: "a web page", body: "<html><body>dss_dolt_version 1</body></html>\n", err: ErrNoSeries},
		{name: "only other series", body: "go_goroutines 28\n", err: ErrNoSeries},
		{name: "binary", body: "\x00\x01\xff\xfe\n\x00dss_dolt_version 1\n", err: ErrNoSeries},
		{name: "a body past the cap", body: body + strings.Repeat("# padding\n", MaxBody/10+1), err: ErrTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(strings.NewReader(tc.body))
			if !errors.Is(err, tc.err) || (tc.err == nil && err != nil) {
				t.Fatalf("Parse(%.80q) error = %v, want %v", tc.body, err, tc.err)
			}
			if show(got) != show(tc.want) {
				t.Errorf("Parse(%.80q) =\n%s\nwant\n%s", tc.body, show(got), show(tc.want))
			}
		})
	}
}

// What Dolt 2.4.2 served, saved as it came: every figure is read from it.
func TestParseDoltsOwn(t *testing.T) {
	t.Parallel()
	f, err := os.Open("testdata/dolt-2.4.2.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	got, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse(testdata/dolt-2.4.2.txt) = %v", err)
	}
	started := time.Unix(1791835308, 0).UTC()
	want := Sample{Version: "2.4.2", Resident: new(uint64(59670528)), MemLimit: new(uint64(1500 << 20)),
		Started: &started, Conns: new(0), GCCycles: new(int64(3))}
	if show(got) != show(want) {
		t.Errorf("Parse(testdata/dolt-2.4.2.txt) =\n%s\nwant\n%s", show(got), show(want))
	}
}

// A reader that fails is an error, not a short sample.
func TestParseReadError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	if _, err := Parse(failing{boom}); !errors.Is(err, boom) {
		t.Errorf("Parse(a reader that fails) = %v, want %v", err, boom)
	}
}

type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }

var versionPattern = regexp.MustCompile(`^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,5}$`)

// The body comes from another process: whatever it holds, Parse returns,
// and what it returns can be printed and sent to a client.
func FuzzParse(f *testing.F) {
	saved, err := os.ReadFile("testdata/dolt-2.4.2.txt")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(saved)
	for _, seed := range []string{
		"", "\n\n", "dss_dolt_version 3.3816578e+07\n", "dss_concurrent_connections{a=\"b\\\"}\"} 4 12\n",
		"process_start_time_seconds 1e300\n", "process_start_time_seconds -1\n", "go_gc_gomemlimit_bytes 9.223372036854776e+18\n",
		"go_gc_duration_seconds_count 0x10\n", "dss_dolt_version{", "process_resident_memory_bytes{a=\"", "\xff{\"\\",
		"dss_concurrent_connections 1\ndss_concurrent_connections 2\n", "go_gc_duration_seconds_count 1_0\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		got, err := Parse(strings.NewReader(string(body)))
		if err != nil {
			if show(got) != show(Sample{}) {
				t.Errorf("Parse(%q) failed with %v and still returned %s", body, err, show(got))
			}
			return
		}
		b, merr := json.Marshal(got)
		if merr != nil {
			t.Fatalf("Parse(%q) returned a sample that cannot be encoded: %v", body, merr)
		}
		if got.Version != "" && !versionPattern.MatchString(got.Version) {
			t.Errorf("Parse(%q).Version = %q, which is not a version", body, got.Version)
		}
		if got.empty() {
			t.Errorf("Parse(%q) returned no figure and no error", body)
		}
		again, _ := Parse(strings.NewReader(string(body)))
		if show(again) != string(b) {
			t.Errorf("Parse(%q) twice = %s, then %s", body, b, show(again))
		}
	})
}
