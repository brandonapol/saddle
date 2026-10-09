// Package scratch keeps saddle's temp files under one root and sweeps them
// (#322). Saddle's gates, its train, its MCP server and the agents it spawns
// all run with TMPDIR and GOTMPDIR under the root (default
// <user cache dir>/saddle/tmp, [tmp] dir in config), so /tmp, often a small
// tmpfs, never fills with go-build dirs and test leftovers.
//
// The sweep is careful about what it deletes. Under the root everything is
// saddle's, but an entry goes only when it is older than the age threshold
// and no live process holds it (its cwd, its binary or an open file is
// inside). Under the OS temp dir only entries with saddle's own prefixes
// (go-build*, e2ebin*, saddle-*) that the user owns are candidates, under the
// same rules; nothing else there is ever touched, and symlinks never are.
package scratch

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

// Defaults for [tmp].
const (
	// DefaultMaxAge is how old an entry must be before a sweep removes it.
	DefaultMaxAge = 60 * time.Minute
	// PressureMaxAge is the age threshold of the harder sweep when free
	// space is low.
	PressureMaxAge = 5 * time.Minute
	// DefaultLowFreePct is the free-space percentage under which the scratch
	// filesystem counts as low.
	DefaultLowFreePct = 15.0
)

// Prefixes are the names of saddle's own entries in the OS temp dir: Go's
// build dirs (from a go test or go build saddle ran), the e2e binaries and
// everything saddle names itself.
var Prefixes = []string{"go-build", "e2ebin", "saddle-"}

// Config is the [tmp] section.
type Config struct {
	// Dir is the scratch root; empty means DefaultRoot.
	Dir string `toml:"dir"`
	// MaxAge is the sweep's age threshold; zero means DefaultMaxAge.
	MaxAge time.Duration `toml:"max_age"`
	// LowFreePct is the low-space threshold in percent; zero means
	// DefaultLowFreePct, negative turns the pressure response off.
	LowFreePct float64 `toml:"low_free_pct"`
}

// LoadConfig reads [tmp] from the user's and the repo's config.toml, the
// repo's winning. It reads the files itself so it works before (and
// without) the config package knowing the section.
func LoadConfig(repo string) Config {
	var c Config
	paths := []string{}
	if home, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(home, "saddle", "config.toml"))
	}
	if repo != "" {
		paths = append(paths, filepath.Join(repo, ".saddle", "config.toml"))
	}
	for _, p := range paths {
		var f struct {
			Tmp Config `toml:"tmp"`
		}
		if _, err := toml.DecodeFile(p, &f); err != nil {
			continue
		}
		if f.Tmp.Dir != "" {
			d := expandHome(f.Tmp.Dir)
			if !filepath.IsAbs(d) && repo != "" {
				d = filepath.Join(repo, d)
			}
			c.Dir = filepath.Clean(d)
		}
		if f.Tmp.MaxAge > 0 {
			c.MaxAge = f.Tmp.MaxAge
		}
		if f.Tmp.LowFreePct != 0 {
			c.LowFreePct = f.Tmp.LowFreePct
		}
	}
	return c
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Root is the scratch root c names, else DefaultRoot.
func (c Config) Root() string {
	if c.Dir != "" {
		return c.Dir
	}
	return DefaultRoot()
}

// Age is c's sweep age threshold.
func (c Config) Age() time.Duration {
	if c.MaxAge > 0 {
		return c.MaxAge
	}
	return DefaultMaxAge
}

// LowFree is c's low-space threshold in percent; 0 means off.
func (c Config) LowFree() float64 {
	switch {
	case c.LowFreePct < 0:
		return 0
	case c.LowFreePct == 0:
		return DefaultLowFreePct
	}
	return c.LowFreePct
}

// osTemp is the OS temp dir as saddle found it at start, before it pointed
// TMPDIR at the scratch root.
var osTemp = func() string {
	if d := os.Getenv("TMPDIR"); d != "" {
		return filepath.Clean(d)
	}
	return "/tmp"
}()

// OSTemp is the OS temp dir saddle started with: $TMPDIR then, else /tmp.
func OSTemp() string { return osTemp }

// DefaultRoot is <user cache dir>/saddle/tmp. Without a cache dir it falls
// back to a per-user dir in the OS temp dir, which the sweep never removes.
func DefaultRoot() string {
	if c, err := os.UserCacheDir(); err == nil && c != "" {
		return filepath.Join(c, "saddle", "tmp")
	}
	return filepath.Join(osTemp, fmt.Sprintf("saddle-tmp-%d", os.Getuid()))
}

// RepoKey is the name of a repo's own dir under the root.
func RepoKey(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return hex.EncodeToString(sum[:])[:12]
}

// Env is TMPDIR and GOTMPDIR pointed at dir.
func Env(dir string) []string { return []string{"TMPDIR=" + dir, "GOTMPDIR=" + dir} }

// Use points this process's TMPDIR and GOTMPDIR at root, creating it, so
// every child saddle starts (gates, the train, agents) inherits it. A TMPDIR
// already under root (a per-run gate dir) is kept.
func Use(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if cur := os.Getenv("TMPDIR"); cur != "" && Within(root, cur) {
		if os.Getenv("GOTMPDIR") == "" {
			return os.Setenv("GOTMPDIR", cur)
		}
		return nil
	}
	return errors.Join(os.Setenv("TMPDIR", root), os.Setenv("GOTMPDIR", root))
}

// Within reports whether p is root or under it.
func Within(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Saddles reports whether name is one of saddle's own OS temp dir entries.
func Saddles(name string) bool {
	for _, p := range Prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Entry is one top-level file or dir the sweep looked at.
type Entry struct {
	Path    string
	Bytes   int64
	Files   int
	ModTime time.Time // the newest of the entry and its direct children
	Saddle  bool      // saddle's own: under the root or saddle-prefixed
	Reason  string    // why it was kept, "" when removed (or removable)
}

// Holders reports the paths live processes hold: their cwd, binary and open
// files. ProcHolders is the real one.
type Holders func() []string

// Options configures a sweep.
type Options struct {
	Root   string        // the scratch root; required
	OSTemp string        // the OS temp dir; "" skips it
	MaxAge time.Duration // zero means DefaultMaxAge
	Now    time.Time     // zero means time.Now()
	DryRun bool          // list, remove nothing
	// Holders lists held paths; nil means ProcHolders.
	Holders Holders
}

// Result is how a sweep went.
type Result struct {
	Removed []Entry // removed, or in a dry run removable
	Kept    []Entry // saddle's entries kept, with why
	Freed   int64   // bytes in Removed
	Errs    []error
}

// Sweep removes saddle's stale scratch: entries under the root (and under a
// repo dir's in it, one level down) and saddle-prefixed entries in the OS
// temp dir, older than MaxAge and held by no live process.
func Sweep(o Options) Result {
	if o.MaxAge <= 0 {
		o.MaxAge = DefaultMaxAge
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.Holders == nil {
		o.Holders = ProcHolders
	}
	var res Result
	cands := Candidates(o.Root, o.OSTemp)
	if len(cands) == 0 {
		return res
	}
	held := o.Holders()
	cutoff := o.Now.Add(-o.MaxAge)
	for _, c := range cands {
		e := inspect(c)
		switch {
		case e.ModTime.After(cutoff):
			e.Reason = "fresh: changed " + o.Now.Sub(e.ModTime).Round(time.Second).String() + " ago"
		case heldIn(held, c):
			e.Reason = "held by a live process"
		}
		if e.Reason != "" {
			res.Kept = append(res.Kept, e)
			continue
		}
		if !o.DryRun {
			if err := os.RemoveAll(c); err != nil {
				e.Reason = "remove failed: " + err.Error()
				res.Kept = append(res.Kept, e)
				res.Errs = append(res.Errs, err)
				continue
			}
		}
		res.Removed = append(res.Removed, e)
		res.Freed += e.Bytes
	}
	return res
}

// Candidates lists the paths a sweep may remove: every top-level entry of
// root except dotfiles, a repo dir's children in place of the repo dir
// itself, and in osTemp the user's own saddle-prefixed entries other than
// root itself. Symlinks are never candidates.
func Candidates(root, osTemp string) []string {
	var out []string
	if root != "" {
		for _, e := range readDir(root) {
			n := e.Name()
			if strings.HasPrefix(n, ".") || e.Type()&fs.ModeSymlink != 0 {
				continue
			}
			p := filepath.Join(root, n)
			if e.IsDir() && isRepoKey(n) {
				for _, c := range readDir(p) {
					if c.Type()&fs.ModeSymlink == 0 {
						out = append(out, filepath.Join(p, c.Name()))
					}
				}
				continue
			}
			out = append(out, p)
		}
	}
	if osTemp != "" && (root == "" || filepath.Clean(osTemp) != filepath.Clean(root)) {
		for _, e := range readDir(osTemp) {
			p := filepath.Join(osTemp, e.Name())
			if !Saddles(e.Name()) || e.Type()&fs.ModeSymlink != 0 || (root != "" && Within(p, root)) {
				continue
			}
			if fi, err := e.Info(); err != nil || !ownedByMe(fi) {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

func readDir(dir string) []fs.DirEntry {
	es, _ := os.ReadDir(dir)
	return es
}

// isRepoKey reports whether name looks like RepoKey's output.
func isRepoKey(name string) bool {
	if len(name) != 12 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

func ownedByMe(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return !ok || int(st.Uid) == os.Getuid()
}

// inspect sizes p and finds its newest change among itself and its direct
// children: a go-build dir a running build writes into keeps changing there.
func inspect(p string) Entry {
	e := Entry{Path: p, Saddle: true}
	fi, err := os.Lstat(p)
	if err != nil {
		return e
	}
	e.ModTime = fi.ModTime()
	if fi.IsDir() {
		for _, c := range readDir(p) {
			if ci, err := c.Info(); err == nil && ci.ModTime().After(e.ModTime) {
				e.ModTime = ci.ModTime()
			}
		}
	}
	e.Bytes, e.Files = size(p)
	return e
}

// size is the bytes and file count under p, not following symlinks.
func size(p string) (int64, int) {
	var n int64
	files := 0
	_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		files++
		if fi, err := d.Info(); err == nil && !d.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n, files
}

func heldIn(held []string, p string) bool {
	for _, h := range held {
		if Within(p, h) {
			return true
		}
	}
	return false
}

// ProcHolders lists every path a process of this user holds, from /proc:
// its cwd, its binary and its open files. Where /proc is missing it lists
// nothing, and the sweep goes by age alone.
func ProcHolders() []string {
	ps, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range ps {
		if p.Name()[0] < '0' || p.Name()[0] > '9' {
			continue
		}
		dir := filepath.Join("/proc", p.Name())
		for _, l := range []string{"cwd", "exe"} {
			if t, err := os.Readlink(filepath.Join(dir, l)); err == nil {
				out = append(out, strings.TrimSuffix(t, " (deleted)"))
			}
		}
		for _, fd := range readDir(filepath.Join(dir, "fd")) {
			if t, err := os.Readlink(filepath.Join(dir, "fd", fd.Name())); err == nil && filepath.IsAbs(t) {
				out = append(out, strings.TrimSuffix(t, " (deleted)"))
			}
		}
	}
	return out
}

// Top lists dir's n biggest top-level entries by bytes, or by file count
// when byCount, each flagged as saddle's or not: everything under a saddle
// root is, and in the OS temp dir what has a saddle prefix.
func Top(dir string, n int, byCount, saddleRoot bool) []Entry {
	var es []Entry
	for _, d := range readDir(dir) {
		if d.Type()&fs.ModeSymlink != 0 {
			continue
		}
		p := filepath.Join(dir, d.Name())
		b, f := size(p)
		es = append(es, Entry{Path: p, Bytes: b, Files: f, Saddle: saddleRoot || Saddles(d.Name())})
	}
	sort.SliceStable(es, func(i, j int) bool {
		if byCount {
			return es[i].Files > es[j].Files
		}
		return es[i].Bytes > es[j].Bytes
	})
	if len(es) > n {
		es = es[:n]
	}
	return es
}

// Describe renders entries as "path (12M, 340 files, saddle's)" items.
func Describe(es []Entry) string {
	parts := make([]string, 0, len(es))
	for _, e := range es {
		owner := "not saddle's"
		if e.Saddle {
			owner = "saddle's"
		}
		parts = append(parts, fmt.Sprintf("%s (%s, %d files, %s)", e.Path, Bytes(e.Bytes), e.Files, owner))
	}
	return strings.Join(parts, "; ")
}

// Bytes renders n as a short human size.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTPE"[exp])
}

// Space is how full a filesystem is.
type Space struct {
	Path        string
	Free, Total uint64
}

// FreePct is the free share in percent; 100 when the size is unknown.
func (s Space) FreePct() float64 {
	if s.Total == 0 {
		return 100
	}
	return float64(s.Free) * 100 / float64(s.Total)
}

func (s Space) String() string {
	return fmt.Sprintf("%s has %s free of %s (%.0f%%)", s.Path, Bytes(int64(s.Free)), Bytes(int64(s.Total)), s.FreePct())
}

// SpaceFunc measures the filesystem holding path. Statfs is the real one.
type SpaceFunc func(path string) (Space, error)

// Statfs measures path's filesystem; free is what an unprivileged user may
// use. A path that doesn't exist yet is measured at its nearest parent.
func Statfs(path string) (Space, error) {
	p := path
	for {
		if _, err := os.Stat(p); err == nil || filepath.Dir(p) == p {
			break
		}
		p = filepath.Dir(p)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return Space{Path: path}, err
	}
	bs := uint64(st.Bsize) //nolint:gosec,unconvert // positive; its type differs by OS
	return Space{Path: path, Free: uint64(st.Bavail) * bs, Total: uint64(st.Blocks) * bs}, nil
}

// Pressure is the outcome of Respond.
type Pressure struct {
	Before, After Space
	Threshold     float64 // percent
	Swept         *Result // the harder sweep, when one ran
	// Top is what uses the most space, the root's and the OS temp dir's,
	// set while still low.
	Top []Entry
}

// Low reports whether free space is still under the threshold.
func (p Pressure) Low() bool { return p.Threshold > 0 && p.After.FreePct() < p.Threshold }

// Message tells the owner what is using the space and how to get out.
func (p Pressure) Message() string {
	if !p.Low() {
		return ""
	}
	freed := ""
	if p.Swept != nil {
		freed = fmt.Sprintf(" Saddle swept its own scratch and freed %s.", Bytes(p.Swept.Freed))
	}
	top := ""
	if len(p.Top) > 0 {
		top = " Biggest: " + Describe(p.Top) + "."
	}
	return fmt.Sprintf("scratch is low on space: %s, under %.0f%%.%s%s Free space there (the entries not saddle's are yours to judge), or point [tmp] dir at a bigger disk; `saddle gc` sweeps again.",
		p.After, p.Threshold, freed, top)
}

// Respond checks the root's filesystem and, when it is under threshold
// percent free, sweeps with the shorter PressureMaxAge and checks again.
// While it stays low it lists the biggest entries.
func Respond(o Options, threshold float64, space SpaceFunc) (Pressure, error) {
	if space == nil {
		space = Statfs
	}
	p := Pressure{Threshold: threshold}
	var err error
	if p.Before, err = space(o.Root); err != nil {
		return p, err
	}
	p.After = p.Before
	if threshold <= 0 || p.Before.FreePct() >= threshold {
		return p, nil
	}
	if o.MaxAge <= 0 || o.MaxAge > PressureMaxAge {
		o.MaxAge = PressureMaxAge
	}
	res := Sweep(o)
	p.Swept = &res
	if p.After, err = space(o.Root); err != nil {
		return p, err
	}
	if p.Low() {
		p.Top = Top(o.Root, 5, false, true)
		if o.OSTemp != "" && !Within(o.Root, o.OSTemp) {
			if s, err := space(o.OSTemp); err == nil && sameFS(s, p.After) {
				p.Top = append(p.Top, Top(o.OSTemp, 5, false, false)...)
			}
		}
		sort.SliceStable(p.Top, func(i, j int) bool { return p.Top[i].Bytes > p.Top[j].Bytes })
		p.Top = p.Top[:min(len(p.Top), 6)]
	}
	return p, nil
}

// sameFS is a cheap guess that two measurements are one filesystem.
func sameFS(a, b Space) bool { return a.Total == b.Total }

// Strays lists saddle-prefixed entries in osTemp changed within since: some
// path still writes saddle's temp files to the OS temp dir instead of the
// root.
func Strays(root, osTemp string, since time.Duration, now time.Time) []string {
	if osTemp == "" || (root != "" && filepath.Clean(root) == filepath.Clean(osTemp)) {
		return nil
	}
	var out []string
	for _, e := range readDir(osTemp) {
		p := filepath.Join(osTemp, e.Name())
		if !Saddles(e.Name()) || e.Type()&fs.ModeSymlink != 0 || (root != "" && Within(p, root)) {
			continue
		}
		if fi, err := e.Info(); err == nil && ownedByMe(fi) && now.Sub(fi.ModTime()) < since {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}
