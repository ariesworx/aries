package doltmetrics

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"
)

// Bounds on what Parse reads. Dolt 2.4.2 serves about 10 KiB in lines of
// at most 250 bytes.
const (
	// MaxBody is the most of a body Parse reads; a longer one is
	// refused whole, with ErrTooLarge.
	MaxBody = 1 << 20
	// MaxLine is the longest line Parse looks at, labels included; a
	// longer one is passed over.
	MaxLine = 4096
)

// Errors of Parse.
var (
	// ErrTooLarge is a body of more than MaxBody bytes.
	ErrTooLarge = errors.New("the body is larger than 1 MiB")
	// ErrNoSeries is a body with none of the series Parse reads: an
	// empty one, a web page, or another program's metrics.
	ErrNoSeries = errors.New("the body holds none of Dolt's series")
)

// Sample is what one read of Dolt's metrics gave. A figure the body did
// not give, or gave as something that cannot be one, is nil, or for the
// version empty.
type Sample struct {
	// Version is Dolt's version, such as "2.4.2".
	Version string
	// Resident is the resident memory of Dolt's process, in bytes.
	Resident *uint64
	// MemLimit is the Go runtime's memory limit (GOMEMLIMIT), in bytes;
	// zero when Dolt runs without one.
	MemLimit *uint64
	// Started is when Dolt's process started.
	Started *time.Time
	// Conns is how many clients are connected.
	Conns *int
	// GCCycles is how many garbage collection cycles the process has
	// completed since it started.
	GCCycles *int64
}

// empty reports whether s holds no figure at all.
func (s Sample) empty() bool {
	return s.Version == "" && s.Resident == nil && s.MemLimit == nil && s.Started == nil && s.Conns == nil && s.GCCycles == nil
}

// The series Parse reads, by name. Everything else in the body is
// passed over unread.
const (
	seriesVersion = iota
	seriesConns
	seriesResident
	seriesStarted
	seriesMemLimit
	seriesGCCycles
	numSeries
)

var seriesNames = map[string]int{
	"dss_dolt_version":              seriesVersion,
	"dss_concurrent_connections":    seriesConns,
	"process_resident_memory_bytes": seriesResident,
	"process_start_time_seconds":    seriesStarted,
	"go_gc_gomemlimit_bytes":        seriesMemLimit,
	"go_gc_duration_seconds_count":  seriesGCCycles,
}

// Bounds on the values believed. A figure past its bound is left out.
const (
	// maxBytes bounds a size in bytes. The Go runtime reports no memory
	// limit as the largest int64, which is past it.
	maxBytes = 1 << 62
	// maxStarted is the last second of the year 9999, the latest time
	// that JSON carries.
	maxStarted = 253402300799
	maxConns   = 1 << 31
	maxCount   = 1 << 62
)

// Parse reads the body of Dolt's metrics endpoint, in Prometheus's text
// format, for the six series a [Sample] holds. The body comes from
// another process and is not trusted: at most MaxBody bytes are read,
// a line longer than MaxLine or that is no sample is passed over, a
// series given more than once is believed in none of its lines, and a
// value that cannot be the figure is left out. It returns ErrTooLarge
// for a longer body and ErrNoSeries for one that gave no figure.
func Parse(r io.Reader) (Sample, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxBody+1))
	if err != nil {
		return Sample{}, err
	}
	if len(body) > MaxBody {
		return Sample{}, ErrTooLarge
	}
	var vals [numSeries]float64
	var seen [numSeries]int
	for len(body) > 0 {
		var line []byte
		line, body, _ = bytes.Cut(body, []byte{'\n'})
		if len(line) > MaxLine {
			continue
		}
		if i, v, ok := parseLine(line); ok {
			vals[i] = v
			seen[i]++
		}
	}
	var s Sample
	// get is series i's value when it was given once and is from zero to
	// limit, which a NaN is not: the comparisons are false for one.
	get := func(i int, limit float64) (float64, bool) {
		v := vals[i]
		if seen[i] != 1 || !(v >= 0 && v <= limit) {
			return 0, false
		}
		return v, true
	}
	if v, ok := get(seriesVersion, math.MaxUint32); ok && v >= 1 {
		// Dolt packs its version into one number: a byte for the major
		// version, a byte for the minor and two for the patch.
		n := uint32(v)
		s.Version = fmt.Sprintf("%d.%d.%d", n>>24, n>>16&0xff, n&0xffff)
	}
	if v, ok := get(seriesConns, maxConns); ok {
		s.Conns = new(int(v))
	}
	if v, ok := get(seriesResident, maxBytes); ok {
		s.Resident = new(uint64(v))
	}
	if v, ok := get(seriesStarted, maxStarted); ok && v >= 1 {
		s.Started = new(time.Unix(int64(v), 0).UTC())
	}
	if seen[seriesMemLimit] == 1 && vals[seriesMemLimit] > maxBytes && !math.IsInf(vals[seriesMemLimit], 0) {
		s.MemLimit = new(uint64(0))
	} else if v, ok := get(seriesMemLimit, maxBytes); ok {
		s.MemLimit = new(uint64(v))
	}
	if v, ok := get(seriesGCCycles, maxCount); ok {
		s.GCCycles = new(int64(v))
	}
	if s.empty() {
		return Sample{}, ErrNoSeries
	}
	return s, nil
}

// parseLine reads one line of the text format, "name value" or
// "name{labels} value", each with an optional timestamp after it, and
// returns which of Parse's series it is and its value. Any other line,
// a comment and another series among them, is not ok.
func parseLine(line []byte) (series int, value float64, ok bool) {
	end := bytes.IndexAny(line, "{ \t")
	if end <= 0 {
		return 0, 0, false
	}
	series, known := seriesNames[string(line[:end])]
	if !known {
		return 0, 0, false
	}
	rest := line[end:]
	if rest[0] == '{' {
		// The labels end at the first brace outside a quoted value, in
		// which a backslash escapes the next byte.
		quoted, closed := false, -1
		for i := 1; i < len(rest) && closed < 0; i++ {
			switch c := rest[i]; {
			case quoted && c == '\\':
				i++
			case c == '"':
				quoted = !quoted
			case c == '}' && !quoted:
				closed = i
			}
		}
		if closed < 0 {
			return 0, 0, false
		}
		rest = rest[closed+1:]
	}
	fields := bytes.Fields(rest)
	if len(fields) == 0 || len(fields) > 2 {
		return 0, 0, false
	}
	// ParseFloat also reads hexadecimal and underscores, which the
	// format does not have.
	if bytes.ContainsAny(fields[0], "xX_pP") {
		return 0, 0, false
	}
	v, err := strconv.ParseFloat(string(fields[0]), 64)
	if err != nil {
		return 0, 0, false
	}
	return series, v, true
}
