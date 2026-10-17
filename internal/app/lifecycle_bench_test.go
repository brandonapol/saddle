package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
)

// benchSetup creates an App for benchmarking without relying on testing.T.
func benchSetup() (*App, string, error) {
	// Create temporary directory
	tmpDir, err := os.MkdirTemp("", "saddle-bench-")
	if err != nil {
		return nil, "", err
	}

	// Initialize git repo
	cmd := exec.Command("git", "-C", tmpDir, "init", "-q", "-b", "main")
	if err := cmd.Run(); err != nil {
		return nil, "", fmt.Errorf("git init failed: %w", err)
	}

	// Create initial files
	billMeterPath := filepath.Join(tmpDir, "billing", "meter.go")
	os.MkdirAll(filepath.Dir(billMeterPath), 0755)
	os.WriteFile(billMeterPath, []byte("package billing\n\nfunc Meter() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "billing", "invoice.go"), []byte("package billing\n\nfunc Invoice() int { return 1 }\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("hi\n"), 0644)

	// Commit initial state
	cmd = exec.Command("git", "-C", tmpDir, "add", "-A")
	if err := cmd.Run(); err != nil {
		return nil, "", fmt.Errorf("git add failed: %w", err)
	}
	cmd = exec.Command("git", "-C", tmpDir, "commit", "-q", "-m", "init")
	if err := cmd.Run(); err != nil {
		return nil, "", fmt.Errorf("git commit failed: %w", err)
	}

	// Open the app
	a, err := Open(tmpDir)
	if err != nil {
		return nil, "", err
	}

	// Configure for testing
	a.Cfg.Test.Cmd = "true"
	a.Tmux = &fakeTmux{windows: map[string]bool{}, sent: map[string][]string{}, names: map[string]string{}}
	a.Cfg.CloseOnLand = true
	a.Cfg.Concurrency = 100 // Allow many tasks to run concurrently for benchmark
	a.AdapterStatus = func() []agent.Status {
		var out []agent.Status
		for _, n := range agent.Names() {
			out = append(out, agent.Status{Name: n, OK: true})
		}
		return out
	}

	return a, tmpDir, nil
}

// BenchmarkGCCounts benchmarks the GCCounts function on a repo with many branches
// and worktrees, comparing the serial path (Leftovers with no deadline) against
// the parallelized 8-worker path (GCCounts with deadline).
// This benchmark runs with a repo containing ~50 task branches/worktrees.
func BenchmarkGCCounts(b *testing.B) {
	// Setup done outside timer
	a, tmpDir, err := benchSetup()
	if err != nil {
		b.Fatalf("benchmark setup failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)
	defer a.Close()

	// Build a repo with ~50 task branches and worktrees.
	// Each task has a branch and worktree. This takes time so it's not
	// included in the benchmark measurements.
	numTasks := 50
	for i := 0; i < numTasks; i++ {
		_, err := a.Spawn(SpawnReq{Title: fmt.Sprintf("task%d", i)})
		if err != nil {
			b.Fatalf("spawn failed: %v", err)
		}
	}

	// Benchmark the serial path (Leftovers with no deadline).
	b.Run("serial", func(b *testing.B) {
		b.ResetTimer() // Only measure the actual Leftovers calls
		for range b.N {
			_, err := a.Leftovers()
			if err != nil {
				b.Fatalf("Leftovers failed: %v", err)
			}
		}
	})

	// Benchmark the parallel 8-worker path (GCCounts with deadline).
	b.Run("parallel-8-worker", func(b *testing.B) {
		b.ResetTimer() // Only measure the actual GCCounts calls
		for range b.N {
			_, _, err := a.GCCounts()
			if err != nil {
				b.Fatalf("GCCounts failed: %v", err)
			}
		}
	})

	// Also benchmark a variant that uses a longer deadline to allow
	// checking everything (similar to serial but still respecting deadline).
	b.Run("deadline-10s", func(b *testing.B) {
		b.ResetTimer() // Only measure the actual leftovers calls
		for range b.N {
			ls, err := a.leftovers(time.Now().Add(10 * time.Second))
			if err != nil {
				b.Fatalf("leftovers failed: %v", err)
			}
			// Count what would be removed/kept
			remove, kept := 0, 0
			for _, l := range ls {
				if l.Keep != "" {
					kept++
				} else {
					remove++
				}
			}
			_ = remove
			_ = kept
		}
	})
}
