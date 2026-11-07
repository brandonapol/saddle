package contract

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/brandonapol/saddle/internal/e2e/fakegh"
	"github.com/brandonapol/saddle/internal/forge"
)

// scenario is one behaviour of GitHub's that saddle depends on. fake builds
// it in a fakegh world and returns the params act needs; a fixture named
// after it holds the same act's calls recorded against GitHub, with its
// params. Both must come to want.
type scenario struct {
	name string
	fake func(f *Fake) Params
	act  func(ctx context.Context, g *forge.GitHub, p Params) (string, error)
	want Outcome
}

// raw runs gh args as a forge write does and types the failure.
func raw(ctx context.Context, g *forge.GitHub, args ...string) (string, error) {
	out, err := g.Run(ctx, args...)
	return out, forge.Classify(err)
}

// stack2 is a two-PR native stack: contract-a on main, contract-b on it.
func stack2(f *Fake) Params {
	prs, st := f.Stack("contract-a", "contract-b")
	return Params{"bottom": prs[0], "top": prs[1], "stack": st}
}

var scenarios = []scenario{
	{
		// GitHub merges a PR in a native stack only through the async
		// merge API; gh pr merge is refused (#838, #820 C8).
		name: "stacked_merge_plain_refused",
		fake: stack2,
		act: func(ctx context.Context, g *forge.GitHub, p Params) (string, error) {
			return raw(ctx, g, "pr", "merge", p["bottom"], "--squash")
		},
		want: Outcome{Kind: "refused", Reason: "stacked_needs_async_merge"},
	},
	{
		// MergePR takes the async API, which answers pending; the PR
		// settles as MERGED a poll or two later.
		name: "stacked_merge_async_accepted",
		fake: stack2,
		act: func(ctx context.Context, g *forge.GitHub, p Params) (string, error) {
			if err := g.MergePR(ctx, p["bottom"], "squash", ""); err != nil {
				return "", err
			}
			pr, err := g.PR(ctx, p["bottom"], forge.PRState)
			return "state=" + pr.State, err
		},
		want: Outcome{Kind: "ok", Value: "state=MERGED"},
	},
	{
		// A PR in a native stack can't change base.
		name: "stacked_base_refused",
		fake: stack2,
		act: func(ctx context.Context, g *forge.GitHub, p Params) (string, error) {
			return "", g.SetPRBase(ctx, p["top"], "main")
		},
		want: Outcome{Kind: "refused", Reason: "stacked_base_locked"},
	},
	{
		// MovePR unstacks, sets the base, and relinks the new stack.
		name: "stacked_move_unlink_base_relink",
		fake: func(f *Fake) Params {
			prs, st := f.Stack("contract-a", "contract-b", "contract-c")
			return Params{"bottom": prs[0], "top": prs[2], "stack": st}
		},
		act: func(ctx context.Context, g *forge.GitHub, p Params) (string, error) {
			err := forge.MovePR(ctx, g, p["top"], "contract-a", forge.NativeStack{Trunk: "main", PRs: []string{p["bottom"], p["top"]}})
			if err != nil {
				return "", err
			}
			pr, err := g.PR(ctx, p["top"], forge.PRBase)
			return "base=" + pr.Base, err
		},
		want: Outcome{Kind: "ok", Value: "base=contract-a"},
	},
	{
		// A PR in the merge queue is locked: its base can't change (#770).
		name: "merge_queue_base_refused",
		fake: func(f *Fake) Params {
			f.PR("contract-a", "main")
			n := f.PR("contract-b", "main")
			f.Update(func(s *fakegh.State) { s.PR(Num(n)).InQueue = true })
			return Params{"pr": n}
		},
		act: func(ctx context.Context, g *forge.GitHub, p Params) (string, error) {
			return "", g.SetPRBase(ctx, p["pr"], "contract-a")
		},
		want: Outcome{Kind: "refused", Reason: "in_merge_queue"},
	},
	{
		// A label that doesn't exist, and isn't one saddle creates, is
		// refused rather than created (#787).
		name: "label_missing",
		fake: func(f *Fake) Params { return Params{"pr": f.PR("contract-a", "main")} },
		act: func(ctx context.Context, g *forge.GitHub, p Params) (string, error) {
			return "", g.AddLabel(ctx, p["pr"], "contract-no-such-label")
		},
		want: Outcome{Kind: "refused", Reason: "label_missing"},
	},
	{
		// The primary (hourly) rate limit, spent (#764).
		name: "rate_limit_primary",
		fake: func(f *Fake) Params {
			n := f.PR("contract-a", "main")
			f.Update(func(s *fakegh.State) { s.Throttle = fakegh.ThrottlePrimary })
			return Params{"pr": n}
		},
		act: func(ctx context.Context, g *forge.GitHub, p Params) (string, error) {
			_, err := g.PR(ctx, p["pr"], forge.PRState)
			return "", err
		},
		want: Outcome{Kind: "rate_limited"},
	},
	{
		// The secondary (abuse) rate limit on a write (#764).
		name: "rate_limit_secondary",
		fake: func(f *Fake) Params {
			f.PR("contract-a", "main")
			f.Update(func(s *fakegh.State) { s.Throttle = fakegh.ThrottleSecondary })
			return Params{}
		},
		act: func(ctx context.Context, g *forge.GitHub, _ Params) (string, error) {
			_, err := g.CreatePR(ctx, forge.NewPR{Base: "main", Head: "contract-a", Title: "again", Body: ""})
			return "", err
		},
		want: Outcome{Kind: "rate_limited"},
	},
	{
		// gh api rate_limit answers while the limit is spent, and its
		// reset is a Unix time.
		name: "rate_limit_reset",
		fake: func(f *Fake) Params {
			f.Update(func(s *fakegh.State) {
				s.Throttle = fakegh.ThrottlePrimary
				s.Quota = fakegh.Quota{Limit: 5000, Remaining: 0, Reset: 1762531200}
			})
			return Params{}
		},
		act: func(ctx context.Context, g *forge.GitHub, _ Params) (string, error) {
			rl, err := forge.ReadRateLimit(ctx, g)
			return fmt.Sprintf("limit=%d remaining=%d reset=%d", rl.Limit, rl.Remaining, rl.Reset.Unix()), err
		},
		want: Outcome{Kind: "ok", Value: "limit=5000 remaining=0 reset=1762531200"},
	},
	{
		// A failing required check blocks the merge.
		name: "required_check_blocks_merge",
		fake: func(f *Fake) Params {
			n := f.PR("contract-a", "main")
			f.Update(func(s *fakegh.State) {
				s.Protected = true
				s.PR(Num(n)).Checks = []fakegh.Check{{Name: "ci", Workflow: "CI", State: fakegh.Fail}}
			})
			return Params{"pr": n}
		},
		act: func(ctx context.Context, g *forge.GitHub, p Params) (string, error) {
			return "", g.MergePR(ctx, p["pr"], "squash", "")
		},
		want: Outcome{Kind: "refused", Reason: "required_check_failing"},
	},
	{
		// saddle/base is a required check only when branch protection
		// lists it: here it doesn't (#838).
		name: "required_checks_without_saddle_base",
		fake: func(f *Fake) Params {
			f.Update(func(s *fakegh.State) { s.Protected, s.Required = true, []string{"ci"} })
			return Params{}
		},
		act: func(ctx context.Context, g *forge.GitHub, _ Params) (string, error) {
			facts, err := g.Facts(ctx, "main")
			return "required=" + strings.Join(facts.RequiredChecks, ","), err
		},
		want: Outcome{Kind: "ok", Value: "required=ci"},
	},
	{
		// ...and here it does.
		name: "required_checks_with_saddle_base",
		fake: func(f *Fake) Params {
			f.Update(func(s *fakegh.State) { s.Protected, s.Required = true, []string{"ci", forge.BaseStatus} })
			return Params{}
		},
		act: func(ctx context.Context, g *forge.GitHub, _ Params) (string, error) {
			facts, err := g.Facts(ctx, "main")
			return "required=" + strings.Join(facts.RequiredChecks, ","), err
		},
		want: Outcome{Kind: "ok", Value: "required=ci,saddle/base"},
	},
	{
		// gh pr checks exits 8 with checks pending, and still prints them.
		name: "checks_pending_exit_8",
		fake: func(f *Fake) Params {
			n := f.PR("contract-a", "main")
			f.Update(func(s *fakegh.State) {
				s.PR(Num(n)).Checks = []fakegh.Check{{Name: "ci", Workflow: "CI", State: fakegh.Pending}}
			})
			return Params{"pr": n}
		},
		act:  checkBuckets,
		want: Outcome{Kind: "ok", Value: "ci=pending"},
	},
	{
		// gh pr checks exits 1 with a check failing, and still prints them.
		name: "checks_failing_exit_1",
		fake: func(f *Fake) Params {
			n := f.PR("contract-a", "main")
			f.Update(func(s *fakegh.State) {
				s.PR(Num(n)).Checks = []fakegh.Check{{Name: "ci", Workflow: "CI", State: fakegh.Fail}}
			})
			return Params{"pr": n}
		},
		act:  checkBuckets,
		want: Outcome{Kind: "ok", Value: "ci=fail"},
	},
	{
		// A squash-merged PR has mergedAt, and its head is not an
		// ancestor of base: the squash commit is new.
		name: "squash_merged_detection",
		fake: func(f *Fake) Params {
			n := f.PR("contract-a", "main")
			f.Must("pr", "merge", n, "--squash")
			return Params{"pr": n, "head": "contract-a"}
		},
		act: func(ctx context.Context, g *forge.GitHub, p Params) (string, error) {
			merged, err := forge.ReadMergedPRs(ctx, g, "main", 10)
			if err != nil {
				return "", err
			}
			got := "not listed"
			for _, m := range merged {
				if strconv.Itoa(m.Number) == p["pr"] {
					got = fmt.Sprintf("listed mergedAt=%t", !m.MergedAt.IsZero())
				}
			}
			out, err := g.Run(ctx, "api", "repos/{owner}/{repo}/compare/main..."+p["head"])
			if err != nil {
				return "", forge.Classify(err)
			}
			var cmp struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal([]byte(out), &cmp); err != nil {
				return "", err
			}
			return got + " compare=" + cmp.Status, nil
		},
		want: Outcome{Kind: "ok", Value: "listed mergedAt=true compare=diverged"},
	},
}

// checkBuckets reads a PR's CI checks as name=bucket.
func checkBuckets(ctx context.Context, g *forge.GitHub, p Params) (string, error) {
	cs, err := g.Checks(ctx, p["pr"])
	var out []string
	for _, c := range cs {
		out = append(out, c.Name+"="+c.Bucket)
	}
	slices.Sort(out)
	return strings.Join(out, ","), err
}
