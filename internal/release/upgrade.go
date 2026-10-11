package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ErrAgentsRunning is why upgrade refuses without --force.
var ErrAgentsRunning = errors.New("agents are running")

// ArchiveName is the release archive for one platform, as
// .goreleaser.yaml's archives.name_template writes it.
func ArchiveName(version, goos, goarch string) string {
	return fmt.Sprintf("saddle_%s_%s_%s.tar.gz", strings.TrimPrefix(version, "v"), goos, goarch)
}

// ExtractBinary returns the file called name from a .tar.gz archive.
func ExtractBinary(archive []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("no %s in the release archive", name)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == name {
			return io.ReadAll(io.LimitReader(tr, 512<<20))
		}
	}
}

// Upgrader replaces the running saddle binary with the latest release.
type Upgrader struct {
	Source       Source
	Current      Info
	GOOS, GOARCH string
	Exe          string // the binary to replace
	StateDB      string // this repo's .saddle/state.db; "" outside a saddle repo
	Force        bool   // upgrade while agents run
	CheckOnly    bool   // show what would change, install nothing
	Running      func() ([]string, error)
	Backup       func(src, dst string) error // store.Backup
	Migrate      func(newExe string) error   // runs the new binary's migrations
	Out          io.Writer
	Now          func() time.Time
}

// Result says what Run did.
type Result struct {
	From, To string
	Upgraded bool
	Backup   string // the state.db backup, if one was made
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Run checks the latest release, shows its changelog, and unless CheckOnly
// installs it: refuse while agents run (unless Force), verify the archive's
// SHA-256, back up state.db, swap the binary (keeping the old one as
// Exe.prev), then run the new binary's migrations.
func (u *Upgrader) Run(ctx context.Context) (Result, error) {
	out := u.Out
	if out == nil {
		out = io.Discard
	}
	rel, err := u.Source.Latest(ctx)
	if err != nil {
		return Result{}, err
	}
	res := Result{From: u.Current.Version, To: rel.Tag}
	latest, ok := ParseSemver(rel.Tag)
	if !ok {
		return res, fmt.Errorf("latest release tag %q is not a version", rel.Tag)
	}
	if cur, ok := ParseSemver(u.Current.Version); !ok {
		fmt.Fprintf(out, "This is a development build (%s, commit %s).\n", u.Current.Version, short(u.Current.Commit))
	} else if Compare(cur, latest) >= 0 {
		fmt.Fprintf(out, "saddle %s is already the latest release (%s).\n", u.Current.Version, rel.Tag)
		return res, nil
	}
	fmt.Fprintf(out, "saddle %s -> %s\n", u.Current.Version, rel.Tag)
	if notes := StripAttribution(rel.Body); notes != "" {
		fmt.Fprintln(out)
		fmt.Fprintln(out, notes)
		fmt.Fprintln(out)
	}
	if u.CheckOnly {
		fmt.Fprintln(out, "Run saddle upgrade to install it.")
		return res, nil
	}
	if u.Running != nil {
		ids, err := u.Running()
		if err != nil {
			return res, fmt.Errorf("check for running agents: %w", err)
		}
		if len(ids) > 0 && !u.Force {
			return res, fmt.Errorf("%w (%s): let them finish or run saddle down first, or pass --force to upgrade anyway (they keep the old binary until restarted)",
				ErrAgentsRunning, strings.Join(ids, ", "))
		}
	}

	name := ArchiveName(rel.Tag, u.GOOS, u.GOARCH)
	archiveURL, sumsURL := "", ""
	for _, a := range rel.Assets {
		switch a.Name {
		case name:
			archiveURL = a.URL
		case ChecksumsName:
			sumsURL = a.URL
		}
	}
	if archiveURL == "" {
		return res, fmt.Errorf("release %s has no %s (no build for %s/%s?)", rel.Tag, name, u.GOOS, u.GOARCH)
	}
	if sumsURL == "" {
		return res, fmt.Errorf("release %s has no %s; refusing to install an unverified binary", rel.Tag, ChecksumsName)
	}
	archive, err := u.Source.Download(ctx, archiveURL)
	if err != nil {
		return res, err
	}
	sumsText, err := u.Source.Download(ctx, sumsURL)
	if err != nil {
		return res, err
	}
	sums, err := ParseChecksums(string(sumsText))
	if err != nil {
		return res, err
	}
	if err := VerifyChecksum(archive, name, sums); err != nil {
		return res, err
	}
	bin, err := ExtractBinary(archive, "saddle")
	if err != nil {
		return res, err
	}
	fmt.Fprintf(out, "Verified %s (sha256).\n", name)

	if u.StateDB != "" && u.Backup != nil {
		if _, err := os.Stat(u.StateDB); err == nil {
			now := time.Now
			if u.Now != nil {
				now = u.Now
			}
			dst := fmt.Sprintf("%s.bak-%s-%s", u.StateDB, unsafeName.ReplaceAllString(u.Current.Version, "_"), now().UTC().Format("20060102T150405Z"))
			if err := u.Backup(u.StateDB, dst); err != nil {
				return res, fmt.Errorf("back up %s (nothing was installed): %w", u.StateDB, err)
			}
			res.Backup = dst
			fmt.Fprintf(out, "Backed up state.db to %s.\n", dst)
		}
	}
	if err := ReplaceBinary(u.Exe, bin); err != nil {
		return res, err
	}
	res.Upgraded = true
	fmt.Fprintf(out, "Installed %s (previous binary kept at %s.prev).\n", u.Exe, u.Exe)
	if u.StateDB != "" && u.Migrate != nil {
		if err := u.Migrate(u.Exe); err != nil {
			return res, fmt.Errorf("migrating state.db with %s failed: %w\nRoll back: mv %s.prev %s && cp %s %s",
				rel.Tag, err, u.Exe, u.Exe, res.Backup, u.StateDB)
		}
	}
	return res, nil
}

// ReplaceBinary atomically replaces the file at exe with data, keeping the
// old file at exe.prev. Running processes keep the old inode.
func ReplaceBinary(exe string, data []byte) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".saddle-upgrade-*")
	if err != nil {
		return fmt.Errorf("write the new binary next to %s: %w", exe, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	prev := exe + ".prev"
	_ = os.Remove(prev)
	if err := os.Link(exe, prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		// No hard links (another filesystem?): copy instead.
		if err := copyFile(exe, prev); err != nil {
			return fmt.Errorf("keep the old binary at %s: %w", prev, err)
		}
	}
	return os.Rename(tmp.Name(), exe)
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o755)
}
