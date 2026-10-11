package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/brandonapol/saddle/internal/doctor"
	"github.com/brandonapol/saddle/internal/release"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/spf13/cobra"
)

// Commit and Date are stamped by release builds alongside Version
// (.goreleaser.yaml, Makefile GO_LDFLAGS). Unset, buildInfo falls back to
// the VCS stamp go build records.
var (
	Commit = ""
	Date   = ""
)

// buildInfo is this binary's version, commit, build date and schema.
func buildInfo() release.Info {
	bi, _ := debug.ReadBuildInfo()
	in := release.Resolve(Version, Commit, Date, bi)
	in.Schema = store.SchemaVersion()
	return in
}

func versionCmd() *cobra.Command {
	var long, asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the saddle version (--long adds commit, build date and state.db schema)",
		Long: `Prints the version alone, for scripts: a release tag (v0.1.0), or a git
description for a build from source. --long adds the commit, build date and the
state.db schema version; --json prints all of it as JSON.

In a saddle repo it also warns when a running saddle up is older than this
binary: restart it to pick up the new version.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := buildInfo()
			out := cmd.OutOrStdout()
			switch {
			case asJSON:
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(in); err != nil {
					return err
				}
			case long:
				fmt.Fprintln(out, in.Long())
			default:
				fmt.Fprintln(out, in.Version)
			}
			warnSkew(cmd.ErrOrStderr(), currentSaddleRoot(), in)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&long, "long", "l", false, "also print the commit, build date and state.db schema")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print version, commit, date and schema as JSON")
	return cmd
}

// currentSaddleRoot is SADDLE_ROOT, else the saddle repo around the working
// directory, else "".
func currentSaddleRoot() string {
	if r := os.Getenv("SADDLE_ROOT"); r != "" {
		return r
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return saddleRoot(wd)
}

// processAlive is release.ProcessAlive; tests fake it.
var processAlive = release.ProcessAlive

// warnSkew prints a warning when the saddle up running in root is older
// than in (#326).
func warnSkew(w io.Writer, root string, in release.Info) {
	if root == "" {
		return
	}
	rec, ok, err := release.ReadUpRecord(filepath.Join(root, ".saddle"))
	if err != nil || !ok {
		return
	}
	if msg := release.SkewWarning(rec, in, processAlive); msg != "" {
		fmt.Fprintln(w, "warning: "+msg)
	}
}

// versionCheck is doctor's line for this binary's build, warning when a
// running saddle up is older (#326).
func versionCheck(root string, in release.Info) doctor.Result {
	const name = "saddle version"
	if rec, ok, err := release.ReadUpRecord(filepath.Join(root, ".saddle")); err == nil && ok {
		if msg := release.SkewWarning(rec, in, processAlive); msg != "" {
			return doctor.Result{Name: name, Status: doctor.Warn, Detail: msg,
				Fix: "quit saddle up and run it again; agents keep working meanwhile"}
		}
	}
	return doctor.Result{Name: name, Status: doctor.OK, Detail: in.Long()}
}

// recordUp notes that saddle up runs from this binary, for warnSkew.
func recordUp(root string) (remove func()) {
	rm, err := release.WriteUpRecord(filepath.Join(root, ".saddle"), buildInfo(), os.Getpid(), time.Now())
	if err != nil {
		return func() {}
	}
	return rm
}
