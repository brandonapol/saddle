package release

import (
	"regexp"
	"strconv"
	"strings"
)

// Semver is a parsed vMAJOR.MINOR.PATCH[-PRE]. Ahead marks a git describe
// of a commit past the tag (v0.1.0-3-gabc1234): newer than the tag, older
// than the next release.
type Semver struct {
	Major, Minor, Patch int
	Pre                 string
	Ahead               bool
}

var (
	semverRE   = regexp.MustCompile(`^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)
	describeRE = regexp.MustCompile(`^(.*)-\d+-g[0-9a-f]+$`)
	tagRE      = regexp.MustCompile(`^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`)
)

// ParseSemver parses a release version, a prerelease, or a git describe of
// a build past a tag. Commit hashes and "dev" are not versions.
func ParseSemver(s string) (Semver, bool) {
	s = strings.TrimSuffix(strings.TrimSpace(s), "-dirty")
	ahead := false
	if m := describeRE.FindStringSubmatch(s); m != nil {
		s, ahead = m[1], true
	}
	m := semverRE.FindStringSubmatch(s)
	if m == nil {
		return Semver{}, false
	}
	v := Semver{Pre: m[4], Ahead: ahead}
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	return v, true
}

// Compare orders a and b: -1, 0 or 1.
func Compare(a, b Semver) int {
	for _, p := range [][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if p[0] != p[1] {
			return cmpInt(p[0], p[1])
		}
	}
	// A prerelease sorts before its release.
	if (a.Pre == "") != (b.Pre == "") {
		if a.Pre == "" {
			return 1
		}
		return -1
	}
	if a.Pre != b.Pre {
		return strings.Compare(a.Pre, b.Pre)
	}
	if a.Ahead != b.Ahead {
		if a.Ahead {
			return 1
		}
		return -1
	}
	return 0
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	return 1
}

// ValidTag reports whether s is a release tag: vX.Y.Z, no prerelease.
func ValidTag(s string) bool { return tagRE.MatchString(s) }
