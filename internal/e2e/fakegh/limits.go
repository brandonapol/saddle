package fakegh

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// throttled is GitHub's rate-limit answer to args while s.Throttle is set,
// or "". gh api rate_limit always answers, as on GitHub.
func throttled(s *State, args []string) string {
	if len(args) >= 2 && args[0] == "api" && strings.TrimPrefix(args[1], "/") == "rate_limit" {
		return ""
	}
	switch s.Throttle {
	case ThrottlePrimary:
		return "GraphQL: API rate limit exceeded for user ID 1. If you reach out to GitHub Support for help, please include the request ID 0000:0000:0000:0000:00000000."
	case ThrottleSecondary:
		return "HTTP 403: You have exceeded a secondary rate limit. Please wait a few minutes before you try again."
	}
	return ""
}

// rateLimit is GET rate_limit. An unset quota is full, resetting in an hour.
func (c *call) rateLimit(jq string) error {
	q := c.s.Quota
	if q.Limit == 0 {
		q = Quota{Limit: 5000, Remaining: 5000, Reset: time.Now().Add(time.Hour).Unix()}
	}
	core := map[string]any{"limit": q.Limit, "used": q.Limit - q.Remaining, "remaining": q.Remaining, "reset": q.Reset}
	return c.print(map[string]any{"resources": map[string]any{"core": core, "graphql": core}, "rate": core}, jq)
}

// requiredChecksPass refuses a merge into a protected default branch while
// a check its protection requires is failing, pending or missing at head: the
// refusal, or "".
func requiredChecksPass(s *State, p *PR, head string) string {
	if !s.Protected || p.Base != s.Default {
		return ""
	}
	cs := p.checksAt(head)
	for _, name := range s.requiredChecks() {
		i := slices.IndexFunc(cs, func(ch Check) bool { return ch.Name == name })
		switch {
		case i < 0 || cs[i].State == Pending:
			return fmt.Sprintf("Required status check %q is expected.", name)
		case cs[i].State == Fail:
			return fmt.Sprintf("Required status check %q is failing.", name)
		}
	}
	return ""
}

// compare is GET compare/BASE...HEAD: how head stands against base.
func (c *call) compare(spec, jq string) error {
	base, head, ok := strings.Cut(spec, "...")
	if !ok {
		return fail("gh: Not Found (HTTP 404)")
	}
	b, h := branchSHA(c.dir, base), branchSHA(c.dir, head)
	if b == "" || h == "" {
		return fail("gh: Not Found (HTTP 404)")
	}
	out, err := git(c.dir, "rev-list", "--left-right", "--count", b+"..."+h)
	if err != nil {
		return err
	}
	f := strings.Fields(out)
	behind, _ := strconv.Atoi(f[0])
	ahead, _ := strconv.Atoi(f[1])
	status := "identical"
	switch {
	case ahead > 0 && behind > 0:
		status = "diverged"
	case ahead > 0:
		status = "ahead"
	case behind > 0:
		status = "behind"
	}
	return c.print(map[string]any{"status": status, "ahead_by": ahead, "behind_by": behind, "total_commits": ahead}, jq)
}
