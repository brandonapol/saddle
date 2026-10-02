package planner

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Stack is a group of tasks that should land as one stack of PRs.
type Stack struct {
	Name   string   // topic shared by every titled task, else the bottom task's ID
	Tasks  []string // task IDs, bottom first, in planner intent order
	Reason string   // why these tasks are grouped, or why the task stands alone
}

// Cluster groups tasks into stacks. It is pure and deterministic.
//
// Two tasks share a stack when one runs After the other, when their claims
// overlap, or when they reference the same issue or epic. Grouping is
// transitive. Claims that overlap a serial glob don't count: every task that
// touches go.mod would otherwise land in one stack, so a bad bottom would put
// unrelated work at risk.
//
// Tasks within a stack, and the stacks themselves, follow planner intent as in
// Check: a topological order of the After graph, ties going to list order.
// Cluster returns the same errors as Check for bad IDs and After references.
func Cluster(tasks []Task, serial []string) ([]Stack, error) {
	g, err := afterGraph(tasks)
	if err != nil {
		return nil, err
	}
	rank := g.intent()

	type link struct {
		i, j   int
		reason string
	}
	var links []link
	for i, t := range tasks {
		for p := range g.pred[i] {
			links = append(links, link{min(i, p), max(i, p), fmt.Sprintf("%s runs after %s", t.ID, tasks[p].ID)})
		}
	}
	own := make([][]string, len(tasks))
	for i, t := range tasks {
		for _, c := range t.Claims {
			if len(matching([]string{c}, serial)) == 0 {
				own[i] = append(own[i], c)
			}
		}
	}
	for i := range tasks {
		for j := i + 1; j < len(tasks); j++ {
			if a, b, ok := firstOverlap(own[i], own[j]); ok {
				links = append(links, link{i, j, fmt.Sprintf("claims overlap: %s %s and %s %s", tasks[i].ID, a, tasks[j].ID, b)})
			}
		}
	}
	firstRef := map[string]int{}
	for j, t := range tasks {
		for _, r := range t.Issues {
			r = issueRef(r)
			if r == "" {
				continue
			}
			if i, ok := firstRef[r]; !ok {
				firstRef[r] = j
			} else if i != j {
				links = append(links, link{i, j, fmt.Sprintf("%s and %s share issue %s", tasks[i].ID, tasks[j].ID, r)})
			}
		}
	}
	slices.SortStableFunc(links, func(a, b link) int {
		return cmp.Or(cmp.Compare(a.i, b.i), cmp.Compare(a.j, b.j))
	})

	parent := make([]int, len(tasks))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	why := map[int][]string{} // root -> reasons for the links that joined it
	for _, l := range links {
		a, b := find(l.i), find(l.j)
		if a == b {
			continue
		}
		parent[b] = a
		why[a] = append(why[a], append(why[b], l.reason)...)
		delete(why, b)
	}

	groups := map[int][]int{}
	for i := range tasks {
		r := find(i)
		groups[r] = append(groups[r], i)
	}
	var out [][]int
	for _, m := range groups {
		slices.SortFunc(m, func(a, b int) int { return rank[a] - rank[b] })
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b []int) int { return rank[a[0]] - rank[b[0]] })

	stacks := make([]Stack, 0, len(out))
	for _, m := range out {
		s := Stack{Name: tasks[m[0]].ID}
		for _, i := range m {
			s.Tasks = append(s.Tasks, tasks[i].ID)
		}
		if topic := sharedTopic(tasks, m); topic != "" {
			s.Name = topic
		}
		if len(m) == 1 {
			s.Reason = "standalone: no After edge, overlapping claim or shared issue links it to another task"
		} else {
			s.Reason = strings.Join(why[find(m[0])], "; ")
		}
		stacks = append(stacks, s)
	}
	return stacks, nil
}

// issueRef normalizes an issue reference: a bare number becomes "#n".
func issueRef(r string) string {
	r = strings.ToLower(strings.TrimSpace(r))
	if r != "" && strings.Trim(r, "0123456789") == "" {
		return "#" + r
	}
	return r
}

// topic is the lowercased one-word prefix of a title like "planner: x", or "".
func topic(title string) string {
	k := strings.Index(title, ":")
	if k <= 0 {
		return ""
	}
	p := strings.TrimSpace(title[:k])
	if p == "" || strings.ContainsAny(p, " \t") {
		return ""
	}
	return strings.ToLower(p)
}

// sharedTopic returns the title topic every member has, or "".
func sharedTopic(tasks []Task, members []int) string {
	var out string
	for _, i := range members {
		t := topic(tasks[i].Title)
		if t == "" || (out != "" && t != out) {
			return ""
		}
		out = t
	}
	return out
}
