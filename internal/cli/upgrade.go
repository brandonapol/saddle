package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/brandonapol/saddle/internal/release"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/spf13/cobra"
)

// releaseSource is where saddle upgrade finds releases; tests fake it.
var releaseSource = func() release.Source {
	tok := os.Getenv("GH_TOKEN")
	if tok == "" {
		tok = os.Getenv("GITHUB_TOKEN")
	}
	return release.GitHub{Token: tok}
}

// selfExe is the binary saddle upgrade replaces; tests point it elsewhere.
var selfExe = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func upgradeCmd() *cobra.Command {
	var force, check bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Install the latest saddle release: shows its changelog, verifies its checksum, backs up state.db",
		Long: `Checks the latest GitHub release of saddle and prints its changelog. Unless
--check, it then replaces this binary with the release built for this platform:

  1. refuses while this repo has running agents (--force upgrades anyway;
     running agents keep the old binary until restarted)
  2. downloads the archive and checksums.txt and verifies the SHA-256;
     a mismatch installs nothing
  3. backs up .saddle/state.db to state.db.bak-<old version>-<time>
  4. swaps the binary, keeping the old one at <binary>.prev
  5. runs the new binary's saddle migrate

Roll back with: mv <binary>.prev <binary>, then copy the backup over
.saddle/state.db (docs/RELEASING.md). Building from source? Use make upgrade.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, err := selfExe()
			if err != nil {
				return err
			}
			root := currentSaddleRoot()
			u := &release.Upgrader{
				Source:    releaseSource(),
				Current:   buildInfo(),
				GOOS:      runtime.GOOS,
				GOARCH:    runtime.GOARCH,
				Exe:       exe,
				Force:     force,
				CheckOnly: check,
				Backup:    store.Backup,
				Out:       cmd.OutOrStdout(),
			}
			if root != "" {
				db := filepath.Join(root, ".saddle", "state.db")
				u.StateDB = db
				u.Running = func() ([]string, error) { return runningAgents(db) }
				u.Migrate = func(newExe string) error {
					c := exec.Command(newExe, "migrate")
					c.Dir = root
					c.Env = append(os.Environ(), "SADDLE_ROOT="+root)
					c.Stdout, c.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
					return c.Run()
				}
			}
			res, err := u.Run(cmd.Context())
			if err != nil {
				return err
			}
			if res.Upgraded {
				fmt.Fprintf(cmd.OutOrStdout(), "Upgraded saddle %s -> %s.\n", res.From, res.To)
				// This process is the old binary; a saddle up still running
				// it needs a restart.
				if root != "" {
					if rec, ok, _ := release.ReadUpRecord(filepath.Join(root, ".saddle")); ok && processAlive(rec.PID) {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: saddle up (pid %d) still runs %s: quit it and run saddle up again.\n", rec.PID, rec.Version)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "upgrade even while agents are running")
	cmd.Flags().BoolVar(&check, "check", false, "only show the latest release and its changelog")
	return cmd
}

// runningAgents lists the workers in the state.db at path that are live:
// running, idle at their prompt, or waiting on the owner.
func runningAgents(path string) ([]string, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	s, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	ts, err := s.Tasks()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, t := range ts {
		if t.Role == store.RoleWorker && slices.Contains([]string{store.Running, store.Idle, store.NeedsYou}, t.Status) {
			ids = append(ids, t.ID)
		}
	}
	return ids, nil
}

func migrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending state.db migrations now, backing the database up first",
		Long: `Opens this repo's .saddle/state.db with this binary, which applies any
migrations it has that the database lacks. A database an older saddle wrote is
first copied to state.db.bak-schema<N>-<time>. saddle upgrade runs this with
the new binary; every other command migrates the same way on first use.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := currentSaddleRoot()
			if root == "" {
				return errors.New("not in a saddle repo (run saddle init)")
			}
			path := filepath.Join(root, ".saddle", "state.db")
			before, err := store.FileSchemaVersion(path)
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(cmd.OutOrStdout(), "No state.db yet; nothing to migrate.")
				return nil
			}
			if err != nil {
				return err
			}
			want := store.SchemaVersion()
			if before > want {
				fmt.Fprintf(cmd.OutOrStdout(), "state.db is at schema %d, newer than this binary's %d: a newer saddle wrote it. Left as is.\n", before, want)
				return nil
			}
			if before == want {
				fmt.Fprintf(cmd.OutOrStdout(), "state.db is at schema %d; nothing to migrate.\n", want)
				return nil
			}
			s, err := store.Open(path)
			if err != nil {
				return err
			}
			if err := s.Close(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Migrated state.db schema %d -> %d.\n", before, want)
			if bs, _ := filepath.Glob(fmt.Sprintf("%s.bak-schema%d-*", path, before)); len(bs) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Backup: %s\n", bs[len(bs)-1])
			}
			return nil
		},
	}
}
