package prgraph

// Reconcile compares desired and actual state and returns the plan.
func Reconcile(in Input) Plan {
	return Plan{}
}

// Collapse is one green, dependency-closed group that could ship as one.
type Collapse struct {
	Root    string   `json:"root"`
	Changes []string `json:"changes"`
}

// Collapsible returns the green, dependency-closed prefixes of each stack.
// It is a view: it never changes the plan.
func Collapsible(in Input) []Collapse {
	return nil
}
