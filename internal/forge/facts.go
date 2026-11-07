package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Facts are what the repo on the forge actually does, probed rather than
// assumed (#822): most stoppages started with saddle assuming GitHub
// behaves one way and reading its refusal as a generic error. Doctor shows
// them; MergePR, MovePR and the reconciler (ADR 0009) act on them.
type Facts struct {
	Base string // the branch the per-branch facts are about; "" reads only the repo's
	// StackedPRs: the repo has GitHub's Stacked PRs, so a PR may sit in a
	// native stack (merge it async, move it with MovePR).
	StackedPRs bool
	// Merge are the merge methods the repo allows.
	Merge MergeSettings
	// AutoMerge: the repo allows auto-merge.
	AutoMerge bool
	// MergeQueue: a merge queue guards Base.
	MergeQueue bool
	// RequiredChecks are the status checks Base requires, sorted, from its
	// rulesets and its branch protection.
	RequiredChecks []string
	// Unread lists the facts the probe couldn't read (usually for want of
	// admin access); they are left zero.
	Unread   []string
	ProbedAt time.Time
}

// FactsTTL is how long probed facts are reused before they are read again.
var FactsTTL = time.Hour

// factsNow is the facts cache's clock; tests replace it.
var factsNow = time.Now

// factProber is a forge that can probe its Facts.
type factProber interface {
	Facts(ctx context.Context, base string) (Facts, error)
}

// ReadFacts probes f's Facts for base, cached per repo for FactsTTL. A
// probe that failed transiently returns that error and isn't cached; a fact
// the probe lacked access to is listed in Unread. A forge that can't probe
// returns ErrUnsupported.
func ReadFacts(ctx context.Context, f Forge, base string) (Facts, error) {
	if p, ok := f.(factProber); ok {
		return p.Facts(ctx, base)
	}
	return Facts{}, fmt.Errorf("read forge facts: %w", ErrUnsupported)
}

// factsCache holds one repo's probed Facts per base.
type factsCache struct {
	mu sync.Mutex
	by map[string]Facts
}

var (
	repoCachesMu sync.Mutex
	repoCaches   = map[string]*factsCache{}
)

// repoFacts is the shared cache of the repo at dir, so every forge New
// makes for it reuses one probe.
func repoFacts(dir string) *factsCache {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	repoCachesMu.Lock()
	defer repoCachesMu.Unlock()
	c := repoCaches[dir]
	if c == nil {
		c = &factsCache{}
		repoCaches[dir] = c
	}
	return c
}

func (c *factsCache) get(base string) (Facts, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.by[base]
	if !ok || factsNow().Sub(f.ProbedAt) >= FactsTTL {
		return Facts{}, false
	}
	return f, true
}

func (c *factsCache) put(f Facts) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.by == nil {
		c.by = map[string]Facts{}
	}
	c.by[f.Base] = f
}

// Facts probes the repo: its settings, whether the stacks API answers, and
// for base its rulesets and branch protection. Four gh calls, then none
// for FactsTTL.
func (g *GitHub) Facts(ctx context.Context, base string) (Facts, error) {
	if g.facts == nil {
		g.facts = &factsCache{}
	}
	if f, ok := g.facts.get(base); ok {
		return f, nil
	}
	f := Facts{Base: base, ProbedAt: factsNow()}
	unread := func(what string, err error) error {
		if Transient(err) {
			return err
		}
		f.Unread = append(f.Unread, fmt.Sprintf("%s: %v", what, err))
		return nil
	}

	ms, auto, err := g.repoSettings(ctx)
	if err != nil {
		if err := unread("merge settings", err); err != nil {
			return Facts{}, err
		}
	}
	f.Merge, f.AutoMerge = ms, auto

	if _, err := g.run(ctx, "api", stacksAPI+"?per_page=1"); err == nil {
		f.StackedPRs = true
	} else if r, ok := RefusalOf(Classify(err)); !ok || (r != NotFound && r != StacksDisabled) {
		if err := unread("stacked PRs", Classify(err)); err != nil {
			return Facts{}, err
		}
	}

	if base != "" {
		if err := g.branchRules(ctx, &f); err != nil {
			if err := unread("rulesets", err); err != nil {
				return Facts{}, err
			}
		}
		p, err := g.BranchProtection(ctx, base)
		if err != nil {
			if err := unread("branch protection", Classify(err)); err != nil {
				return Facts{}, err
			}
		}
		for _, c := range p.Checks {
			if !slices.Contains(f.RequiredChecks, c) {
				f.RequiredChecks = append(f.RequiredChecks, c)
			}
		}
		slices.Sort(f.RequiredChecks)
	}
	g.facts.put(f)
	return f, nil
}

// repoSettings reads the merge methods and auto-merge in one repo read.
func (g *GitHub) repoSettings(ctx context.Context) (MergeSettings, bool, error) {
	out, err := g.run(ctx, "api", "repos/{owner}/{repo}")
	if err != nil {
		return MergeSettings{}, false, Classify(err)
	}
	return parseRepoSettings(out)
}

// branchRules reads the rulesets that apply to f.Base: a merge queue and
// required status checks.
func (g *GitHub) branchRules(ctx context.Context, f *Facts) error {
	out, err := g.run(ctx, "api", "repos/{owner}/{repo}/rules/branches/"+f.Base)
	if err != nil {
		if r, _ := RefusalOf(Classify(err)); r == NotFound {
			return nil
		}
		return Classify(err)
	}
	var rules []struct {
		Type   string `json:"type"`
		Params struct {
			Checks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(out), &rules); err != nil {
		return fmt.Errorf("parse gh api output: %w", err)
	}
	for _, r := range rules {
		switch r.Type {
		case "merge_queue":
			f.MergeQueue = true
		case "required_status_checks":
			for _, c := range r.Params.Checks {
				if !slices.Contains(f.RequiredChecks, c.Context) {
					f.RequiredChecks = append(f.RequiredChecks, c.Context)
				}
			}
		}
	}
	return nil
}
