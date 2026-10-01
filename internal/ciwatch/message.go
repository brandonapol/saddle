package ciwatch

import (
	"fmt"
	"strings"
)

func (o Origin) name() string {
	if o.PR != "" {
		return o.PR + " (" + o.Branch + ")"
	}
	return o.Branch
}

// Message describes the failure and how to fix it, with the log tail.
func (f Failed) Message() string {
	var b strings.Builder
	fmt.Fprintf(&b, "CI failed on %s: check %q", f.name(), f.Check.Label())
	if f.Step != "" {
		fmt.Fprintf(&b, " in step %q", f.Step)
	}
	b.WriteString(".")
	if f.RunURL != "" {
		b.WriteString("\nRun: " + f.RunURL)
	}
	switch {
	case f.LogTail != "":
		fmt.Fprintf(&b, "\nEnd of the failed log:\n%s", f.LogTail)
	case f.LogErr != "":
		b.WriteString("\nThe log could not be fetched: " + f.LogErr)
	}
	fmt.Fprintf(&b, "\nFix it on %s, commit, and call the saddle done tool again.", f.Branch)
	return b.String()
}

// Message says the check passes again.
func (r Recovered) Message() string {
	return fmt.Sprintf("CI is passing again on %s: check %q passed.", r.name(), r.Check.Label())
}
