package doctor

import (
	"errors"
	"testing"
)

func TestInhibitCheckWarnsWithoutAnInhibitor(t *testing.T) {
	none := func(string) (string, error) { return "", errors.New("not found") }
	if r := inhibitCheck("linux", none); r.Status != Warn || r.Fix == "" {
		t.Errorf("no systemd-inhibit: %+v, want a warning with a fix", r)
	}
	have := func(string) (string, error) { return "/usr/bin/caffeinate", nil }
	if r := inhibitCheck("darwin", have); r.Status != OK {
		t.Errorf("caffeinate on PATH: %+v, want ok", r)
	}
	if r := inhibitCheck("linux", have); r.Status != OK || r.About == "" {
		t.Errorf("systemd-inhibit on PATH: %+v, want ok with About", r)
	}
}
