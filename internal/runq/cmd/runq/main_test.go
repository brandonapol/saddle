package main

import (
	"path/filepath"
	"testing"
)

// Smoke test for the spike CLI the e2e journeys drive as two contenders.
func TestRunqSmoke(t *testing.T) {
	db := filepath.Join(t.TempDir(), "runq.db")
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no command", []string{"-db", db}, 2},
		{"bad flag", []string{"-nope"}, 2},
		{"unknown command", []string{"-db", db, "bogus"}, 2},
		{"run without class", []string{"-db", db, "run", "--", "true"}, 2},
		{"run without command", []string{"-db", db, "run", "-class", "go-test"}, 2},
		{"status on an empty queue", []string{"-db", db, "status"}, 0},
		{"run succeeds", []string{"-db", db, "run", "-class", "go-test", "--", "true"}, 0},
		{"run propagates the exit code", []string{"-db", db, "run", "-class", "go-test", "--", "sh", "-c", "exit 7"}, 7},
		{"status after runs", []string{"-db", db, "status"}, 0},
	}
	for _, c := range cases {
		if got := run(c.args); got != c.want {
			t.Errorf("%s: run(%v) = %d, want %d", c.name, c.args, got, c.want)
		}
	}
}
