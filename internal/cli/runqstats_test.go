package cli

import (
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/runq"
)

func TestParseSince(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"12h", 12 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"0d", 0, true},
		{"-1h", 0, true},
		{"soon", 0, true},
	}
	for _, tt := range tests {
		got, err := parseSince(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("parseSince(%q) = %v, %v", tt.in, got, err)
		}
	}
}

func TestStatsSummary(t *testing.T) {
	tests := []struct {
		name string
		st   []runq.ClassStats
		want string
	}{
		{"none", nil, ""},
		{"two classes", []runq.ClassStats{
			{Class: "go-test", Runs: 42, HeldP50MS: 190000, AvgCores: 2.1},
			{Class: "flutter-test", Runs: 3, HeldP50MS: 45000, AvgCores: 0.96},
		}, "heavy runs (7d): go-test 42 runs p50 3m10s 2.1 cores; flutter-test 3 runs p50 45s 1.0 cores"},
	}
	for _, tt := range tests {
		if got := statsSummary(tt.st); got != tt.want {
			t.Errorf("%s: got %q want %q", tt.name, got, tt.want)
		}
	}
}
