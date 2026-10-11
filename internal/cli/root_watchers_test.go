package cli

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/trust"
)

// freeAddr is a loopback host:port nothing listens on right now.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func listening(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// TestStartWatchersServesRemoteControl: with [remote] enabled in the user
// config, startWatchers brings the managed server up on loopback, a call
// without a token is refused, and the returned stop func closes the
// listener and waits.
func TestStartWatchersServesRemoteControl(t *testing.T) {
	a, _ := landedPR(t)
	t.Setenv("SADDLE_REMOTE_DIR", t.TempDir())
	addr := freeAddr(t)
	cfgDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "saddle")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "[remote]\nenabled = true\nlisten = \"" + addr + "\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := trust.Default()
	if err != nil {
		t.Fatal(err)
	}
	r, err := trust.Identify(a.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remember(r); err != nil {
		t.Fatal(err)
	}

	stop := startWatchers(t.Context(), a)
	deadline := time.Now().Add(10 * time.Second)
	for !listening(addr) {
		if time.Now().After(deadline) {
			stop()
			t.Fatal("remote control never listened with [remote] enabled")
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp, err := http.Post("http://"+addr+"/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", resp.StatusCode)
	}

	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("watchers did not stop")
	}
	if listening(addr) {
		t.Fatal("remote control still listening after stop")
	}
}

// TestStartWatchersRemoteControlOffByDefault: with [remote] present but
// not enabled, startWatchers opens no listener.
func TestStartWatchersRemoteControlOffByDefault(t *testing.T) {
	a, _ := landedPR(t)
	t.Setenv("SADDLE_REMOTE_DIR", t.TempDir())
	addr := freeAddr(t)
	cfgDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "saddle")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "[remote]\nlisten = \"" + addr + "\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := startWatchers(t.Context(), a)
	defer stop()
	time.Sleep(300 * time.Millisecond)
	if listening(addr) {
		t.Fatal("remote control listened while not enabled")
	}
}
