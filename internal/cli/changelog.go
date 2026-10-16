package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/release"
)

// changelogExec runs git in dir and returns trimmed stdout; tests fake it.
var changelogExec = func(dir, name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	c.Dir = dir
	var eb strings.Builder
	c.Stderr = &eb
	b, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(eb.String()))
	}
	return strings.TrimSpace(string(b)), nil
}

// changelogMerged lists the PRs merged into base through the repo's forge,
// as [forge] selects it; tests fake it.
var changelogMerged = func(ctx context.Context, dir, base string) ([]forge.MergedPR, error) {
	cfg, err := config.Load(dir)
	if err != nil {
		cfg = config.Default()
	}
	return forge.ReadMergedPRs(ctx, forge.New(cfg.Forge, dir), base, 1000)
}

func changelogCmd() *cobra.Command {
	var version, since, base, date, extract string
	var write bool
	cmd := &cobra.Command{
		Use:   "changelog --version vX.Y.Z [--write]",
		Short: "Generate release notes from the PRs merged since the last tag",
		Long: `Lists the pull requests merged into --base since --since (default: the newest
v* tag; every merged PR when there is none) through the forge (GitHub only), groups them by label or
conventional prefix (Breaking changes, Features, Fixes, Performance,
Documentation, Build and CI, Other changes) and prints a CHANGELOG.md section
for --version. AI co-author and attribution lines are never included.

--write adds the section to CHANGELOG.md at the repo root, newest first.
--extract vX.Y.Z prints that version's section from CHANGELOG.md, which is
what the release workflow publishes as the GitHub Release notes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			top, err := changelogExec(wd, "git", "rev-parse", "--show-toplevel")
			if err != nil {
				return err
			}
			path := filepath.Join(top, "CHANGELOG.md")
			if extract != "" {
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				notes, ok := release.Extract(string(b), extract)
				if !ok {
					return fmt.Errorf("CHANGELOG.md has no section for %s (run saddle changelog --version %s --write)", extract, extract)
				}
				fmt.Fprint(cmd.OutOrStdout(), notes)
				return nil
			}
			if !release.ValidTag(version) {
				return fmt.Errorf("--version must be a release tag like v0.1.0, got %q", version)
			}
			prs, err := mergedSince(top, base, since)
			if err != nil {
				return err
			}
			if date == "" {
				date = time.Now().UTC().Format(time.DateOnly)
			}
			section := release.Render(version, date, prs)
			if !write {
				fmt.Fprint(cmd.OutOrStdout(), section)
				return nil
			}
			old, err := os.ReadFile(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			updated, err := release.Insert(string(old), section)
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added %s (%d PRs) to %s. Review it, then commit it through a PR.\n", version, len(prs), path)
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "the release this section is for (vX.Y.Z)")
	cmd.Flags().StringVar(&since, "since", "", "the previous release tag (default: the newest v* tag)")
	cmd.Flags().StringVar(&base, "base", "main", "the branch PRs were merged into")
	cmd.Flags().StringVar(&date, "date", "", "the release date (default: today, UTC)")
	cmd.Flags().BoolVar(&write, "write", false, "add the section to CHANGELOG.md instead of printing it")
	cmd.Flags().StringVar(&extract, "extract", "", "print this version's section from CHANGELOG.md")
	return cmd
}

// mergedSince lists the PRs merged into base after the tag since was made
// (the newest v* tag when since is ""; all of them when there is no tag).
func mergedSince(dir, base, since string) ([]release.PR, error) {
	if since == "" {
		if t, err := changelogExec(dir, "git", "describe", "--tags", "--abbrev=0", "--match", "v[0-9]*"); err == nil {
			since = t
		}
	}
	var after time.Time
	if since != "" {
		at, err := changelogExec(dir, "git", "log", "-1", "--format=%cI", since)
		if err != nil {
			return nil, err
		}
		if after, err = time.Parse(time.RFC3339, at); err != nil {
			return nil, fmt.Errorf("date of %s: %w", since, err)
		}
	}
	merged, err := changelogMerged(context.Background(), dir, base)
	if errors.Is(err, forge.ErrUnsupported) {
		return nil, fmt.Errorf("saddle changelog needs a GitHub forge to list merged PRs: %w", err)
	}
	if err != nil {
		return nil, err
	}
	var prs []release.PR
	for _, r := range merged {
		if !r.MergedAt.After(after) {
			continue
		}
		prs = append(prs, release.PR{Number: r.Number, Title: r.Title, Labels: r.Labels})
	}
	return prs, nil
}
