package proto

import (
	"testing"
	"time"
)

func TestParseDate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"", time.Time{}, true},
		{"2026-11-02", time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), true},
		{"2026-11-02T09:30:00Z", time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC), true},
		{"2026-11-02T09:30:00+02:00", time.Date(2026, 11, 2, 7, 30, 0, 0, time.UTC), true},
		{"tomorrow", time.Time{}, false},
		{"2026-13-01", time.Time{}, false},
		{"02/11/2026", time.Time{}, false},
		// The zero time would clear the date, and the store keeps none
		// before 1970.
		{"0001-01-01", time.Time{}, false},
		{"1969-12-31T23:59:59Z", time.Time{}, false},
		{"9999-12-31", time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), true},
	}
	for _, tc := range tests {
		got, err := ParseDate(tc.in)
		if (err == nil) != tc.ok || !got.Equal(tc.want) {
			t.Errorf("ParseDate(%q) = %v, %v; want %v, ok %v", tc.in, got, err, tc.want, tc.ok)
		}
		if err == nil && got.Location() != time.UTC {
			t.Errorf("ParseDate(%q) is in %v, want UTC", tc.in, got.Location())
		}
	}
}

// A date a client sends is parsed by the server: whatever it is, it is
// a UTC time or an error, and a time it accepts reads back the same.
func FuzzParseDate(f *testing.F) {
	for _, s := range []string{"", "2026-11-02", "2026-11-02T09:30:00Z", "2026-11-02T09:30:00.123456+05:45", "9999-12-31", "x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := ParseDate(s)
		if err != nil {
			return
		}
		if got.Location() != time.UTC {
			t.Fatalf("ParseDate(%q) = %v, not UTC", s, got)
		}
		if s == "" {
			return
		}
		again, err := ParseDate(got.Format(time.RFC3339Nano))
		if err != nil || !again.Equal(got) {
			t.Fatalf("ParseDate(%q) = %v, which reads back as %v, %v", s, got, again, err)
		}
	})
}
