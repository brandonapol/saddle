package sre

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"

	"github.com/brandonapol/saddle/internal/claims"
)

// LaneKind names one investigation lane.
type LaneKind string

// The lanes. Code and logs always run; infra and deps are picked from the
// incident and the checks, so a plan has 3 or 4 lanes.
const (
	LaneCode  LaneKind = "code"
	LaneLogs  LaneKind = "logs"
	LaneInfra LaneKind = "infra"
	LaneDeps  LaneKind = "deps"
)

// maxClaims caps the code lane's claims; a long trace names many frames.
const maxClaims = 20

// Lane is one read-only investigator's assignment. Only the code lane holds
// path claims; the others touch no files (#547).
type Lane struct {
	Kind      LaneKind
	Focus     string
	Claims    []string // repo-relative files the incident names (code lane only)
	Checks    []string // preemptive checks this lane follows up
	Endpoints []string // config endpoints this lane looks at
}

var focus = map[LaneKind]string{
	LaneCode: "Code diffs and deploys: what changed in the code and in what shipped since the last good state. " +
		"Read git log and diffs, deploy and release history, and the files the incident names.",
	LaneLogs: "Logs and errors: error messages, stack traces, error rates and when they started. " +
		"Line the timeline up against deploys and config changes.",
	LaneInfra: "Infrastructure: hosts, clusters, databases, queues, networking and capacity. " +
		"Start from the failed preemptive checks and the endpoints listed below.",
	LaneDeps: "Dependencies: third-party services, upstream APIs, certificates and library upgrades " +
		"(dependency manifests and lock files, provider status pages).",
}

func words(ws ...string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b(?:` + strings.Join(ws, "|") + `)\b`)
}

// Signal words per optional lane. Logs words only route checks; the logs
// lane always runs.
var signals = map[LaneKind]*regexp.Regexp{
	LaneInfra: words(`database`, `db`, `postgres\w*`, `mysql`, `redis`, `memcached?`, `kafka`, `rabbitmq`, `queues?`,
		`kube\w*`, `k8s`, `pods?`, `nodes?`, `clusters?`, `containers?`, `cpu`, `memory`, `oom\w*`, `disks?`,
		`network\w*`, `dns`, `latency`, `load ?balancer`, `connections?`, `hosts?`, `capacity`, `ec2`, `vm`, `terraform`),
	LaneDeps: words(`dependenc(?:y|ies)`, `upstream`, `third[- ]party`, `vendor\w*`, `librar(?:y|ies)`, `packages?`,
		`sdk`, `upgrad\w*`, `bump\w*`, `stripe`, `twilio`, `rate[- ]limit\w*`, `429`, `certificates?`, `tls`, `ssl`,
		`cve[-\d]*`, `npm`, `yarn`, `pip`, `cargo`, `go\.mod`, `go\.sum`, `outdated`, `audit`),
	LaneLogs: words(`logs?`, `journalctl`, `sentry`, `errors?`, `exceptions?`, `stack ?traces?`),
}

// PlanLanes picks 3 or 4 disjoint lanes for inc. results are the preemptive
// checks' outcomes; a failed check counts toward its lane. repo resolves the
// incident's paths into the code lane's claims.
func PlanLanes(inc Incident, cfg *Config, results []CheckResult, repo fs.FS) []Lane {
	if cfg == nil {
		cfg = &Config{}
	}
	score := map[LaneKind]int{
		LaneInfra: len(signals[LaneInfra].FindAllString(inc.Text, -1)),
		LaneDeps:  len(signals[LaneDeps].FindAllString(inc.Text, -1)),
	}
	status := map[string]string{}
	for _, r := range results {
		status[r.Name] = r.Status
	}
	for _, ch := range cfg.Checks {
		if status[ch.Name] == CheckFailed {
			score[checkLane(ch)] += 2
		}
	}
	kinds := []LaneKind{LaneCode, LaneLogs}
	for _, k := range []LaneKind{LaneInfra, LaneDeps} {
		if score[k] > 0 {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) == 2 {
		kinds = append(kinds, LaneInfra) // nothing pointed elsewhere: infra is the likeliest third
	}
	lanes := make([]Lane, len(kinds))
	idx := map[LaneKind]int{}
	for i, k := range kinds {
		lanes[i] = Lane{Kind: k, Focus: focus[k]}
		idx[k] = i
	}
	lanes[idx[LaneCode]].Claims = resolveClaims(repo, inc.Paths)
	for _, ch := range cfg.Checks {
		k := checkLane(ch)
		if _, ok := idx[k]; !ok {
			k = lanes[2].Kind // the planned optional lane
		}
		lanes[idx[k]].Checks = append(lanes[idx[k]].Checks, ch.Name)
	}
	for _, n := range sortedKeys(cfg.Endpoints) {
		k := LaneInfra
		if _, ok := idx[k]; !ok {
			k = LaneDeps
		}
		lanes[idx[k]].Endpoints = append(lanes[idx[k]].Endpoints, n)
	}
	return lanes
}

// checkLane routes a check to the lane its words point at, infra by default.
func checkLane(ch Check) LaneKind {
	text := strings.Join(append([]string{ch.Name, ch.Description, ch.Surface}, ch.Command...), " ")
	best, n := LaneInfra, len(signals[LaneInfra].FindAllString(text, -1))
	for _, k := range []LaneKind{LaneLogs, LaneDeps} {
		if m := len(signals[k].FindAllString(text, -1)); m > n {
			best, n = k, m
		}
	}
	return best
}

// resolveClaims maps written paths to repo files: a frame such as
// /app/internal/x.go or github.com/org/repo/internal/x.go resolves to the
// longest suffix that exists in repo. Paths not in the repo are dropped.
func resolveClaims(repo fs.FS, paths []string) []string {
	if repo == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		f, ok := resolvePath(repo, p)
		if !ok || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
		if len(out) == maxClaims {
			break
		}
	}
	return out
}

func resolvePath(repo fs.FS, p string) (string, bool) {
	p = strings.Trim(path.Clean(strings.ReplaceAll(p, "\\", "/")), "/")
	segs := strings.Split(p, "/")
	for i := range segs {
		c := strings.Join(segs[i:], "/")
		if !fs.ValidPath(c) {
			continue
		}
		if st, err := fs.Stat(repo, c); err == nil && st.Mode().IsRegular() {
			return claims.Clean(c), true
		}
	}
	return "", false
}

// CheckDisjoint reports an error when two lanes' claims could cover a common path.
func CheckDisjoint(lanes []Lane) error {
	for i, a := range lanes {
		for _, b := range lanes[i+1:] {
			for _, ca := range a.Claims {
				for _, cb := range b.Claims {
					if claims.Overlap(ca, cb) {
						return fmt.Errorf("lanes %s and %s overlap: %s and %s", a.Kind, b.Kind, ca, cb)
					}
				}
			}
		}
	}
	return nil
}
