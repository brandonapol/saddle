package agent

import (
	"context"
	"errors"
	"testing"
)

// #798: the probe tells a dead login (a 401, "Please run /login") from a
// working one and from a probe that failed for another reason.
func TestProbeAuth(t *testing.T) {
	defer func(f func(context.Context, string, ...string) (string, error)) { probeRun = f }(probeRun)
	for _, c := range []struct {
		out  string
		err  error
		want error
	}{
		{"ok\n", nil, nil},
		{"API Error: 401 {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"OAuth token has expired.\"}} · Please run /login", errors.New("exit status 1"), ErrLoggedOut},
		{"network unreachable", errors.New("exit status 1"), nil},
	} {
		probeRun = func(context.Context, string, ...string) (string, error) { return c.out, c.err }
		err := ProbeAuth("claude", "")
		switch {
		case c.want == nil && c.err == nil && err != nil:
			t.Errorf("%q: %v, want nil", c.out, err)
		case c.want != nil && !errors.Is(err, c.want):
			t.Errorf("%q: %v, want %v", c.out, err, c.want)
		case c.err != nil && c.want == nil && (err == nil || errors.Is(err, ErrLoggedOut)):
			t.Errorf("%q: %v, want an inconclusive error", c.out, err)
		}
	}
	if err := ProbeAuth("grok", ""); !errors.Is(err, ErrNoProbe) {
		t.Errorf("grok: %v, want ErrNoProbe", err)
	}
}
