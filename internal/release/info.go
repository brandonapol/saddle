package release

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// Info is what a binary knows about its own build.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	// Schema is the state.db schema version the binary writes.
	Schema int `json:"schema"`
}

// Resolve fills in the commit and build date the release build stamps with
// -ldflags, falling back to the VCS stamp go build records (a dirty tree
// marks the commit -dirty), then "unknown".
func Resolve(version, commit, date string, bi *debug.BuildInfo) Info {
	in := Info{Version: version, Commit: commit, Date: date}
	if bi != nil {
		var rev, at string
		dirty := false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				at = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if in.Commit == "" && rev != "" {
			in.Commit = rev
			if dirty {
				in.Commit += "-dirty"
			}
		}
		if in.Date == "" {
			in.Date = at
		}
	}
	if in.Version == "" {
		in.Version = "dev"
	}
	if in.Commit == "" {
		in.Commit = "unknown"
	}
	if in.Date == "" {
		in.Date = "unknown"
	}
	return in
}

// Long is the one-line description saddle version --long and doctor print.
func (i Info) Long() string {
	return fmt.Sprintf("saddle %s (commit %s, built %s, state.db schema %d)", i.Version, short(i.Commit), i.Date, i.Schema)
}

func short(commit string) string {
	base, dirty := strings.CutSuffix(commit, "-dirty")
	if len(base) > 12 {
		base = base[:12]
	}
	if dirty {
		base += "-dirty"
	}
	return base
}
