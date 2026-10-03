package cli

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// withTmux adds --tmux to the status command: a one-line segment for a tmux
// status-right, e.g. set -g status-right '#(saddle status --tmux)'.
func withTmux(cmd *cobra.Command) *cobra.Command {
	var (
		tmuxSeg, color bool
		width          int
	)
	full := cmd.RunE
	tmuxRun := withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
		s, err := loadSegment(a, time.Now())
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), s.render(color, width))
		return nil
	})
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if tmuxSeg {
			return tmuxRun(cmd, args)
		}
		return full(cmd, args)
	}
	cmd.Flags().BoolVar(&tmuxSeg, "tmux", false, "print a one-line tmux status segment (reads only local state)")
	cmd.Flags().BoolVar(&color, "color", false, "with --tmux, color what needs attention with tmux #[fg=…] codes")
	cmd.Flags().IntVar(&width, "width", 0, "with --tmux, fit the segment in this many columns (0: no limit)")
	return cmd
}

// segment is what the tmux status segment shows.
type segment struct {
	Running, NeedsYou, Queued int
	AutoMerge                 string      // on, off or stopped
	Usage                     usage.State // the worst plan-limit window
	UsageNote                 string      // that window's name and percent, when not OK
}

// loadSegment reads the segment from the store and .saddle/ files. It runs
// no git and asks GitHub nothing, so tmux can poll it every few seconds.
func loadSegment(a *app.App, now time.Time) (segment, error) {
	var s segment
	ts, err := mcpserver.Tasks(a)
	if err != nil {
		return s, err
	}
	for _, t := range ts {
		if t.ID == app.OrchestratorID {
			continue
		}
		switch t.Status {
		case store.Running, store.Idle:
			s.Running++
		case store.NeedsYou:
			s.NeedsYou++
		}
	}
	q, err := a.Queue()
	if err != nil {
		return s, err
	}
	s.Queued = len(q)
	am, err := a.AutomergeState()
	if err != nil {
		return s, err
	}
	switch {
	case am.Stopped != "":
		s.AutoMerge = "stopped"
	case am.Enabled:
		s.AutoMerge = "on"
	default:
		s.AutoMerge = "off"
	}
	e, err := a.Limits(now)
	if err != nil {
		return s, err
	}
	s.Usage = e.State
	for _, w := range []usage.WindowEstimate{e.FiveHour, e.Weekly} {
		if w.State == e.State && w.State != usage.OK {
			s.UsageNote = fmt.Sprintf("%s %.0f%%", w.Name, w.Percent*100)
			break
		}
	}
	return s, nil
}

// segPart is one piece of the segment; prio orders what a width cap drops
// first (highest first).
type segPart struct {
	text, color string
	prio        int
}

// render joins the segment's parts. With color, the parts that need
// attention carry tmux style codes; width > 0 drops parts, least useful
// first, until the plain text fits, then clips.
func (s segment) render(color bool, width int) string {
	parts := []segPart{
		{text: fmt.Sprintf("%d run", s.Running), prio: 2},
		{text: fmt.Sprintf("%d you", s.NeedsYou), prio: 0},
		{text: fmt.Sprintf("%d queued", s.Queued), prio: 3},
		{text: "am " + s.AutoMerge, prio: 4},
	}
	if s.AutoMerge == "" {
		parts[3].text = "am off"
	}
	if s.NeedsYou > 0 {
		parts[1].color = "red"
	}
	if s.AutoMerge == "stopped" {
		parts[3].color = "yellow"
	}
	switch s.Usage {
	case usage.Warn:
		parts = append(parts, segPart{text: strings.TrimSpace("! " + s.UsageNote), color: "yellow", prio: 1})
	case usage.Over:
		parts = append(parts, segPart{text: strings.TrimSpace("!! " + s.UsageNote), color: "red", prio: 1})
	}
	const sep = " · "
	plain := func(ps []segPart) string {
		ts := make([]string, len(ps))
		for i, p := range ps {
			ts[i] = p.text
		}
		return strings.Join(ts, sep)
	}
	if width > 0 {
		for utf8.RuneCountInString(plain(parts)) > width && len(parts) > 1 {
			drop := 0
			for i, p := range parts {
				if p.prio > parts[drop].prio {
					drop = i
				}
			}
			parts = append(parts[:drop], parts[drop+1:]...)
		}
		if len(parts) == 1 {
			parts[0].text = trunc(parts[0].text, width)
		}
	}
	if !color {
		return plain(parts)
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.text
		if p.color != "" {
			out[i] = "#[fg=" + p.color + "]" + p.text + "#[default]"
		}
	}
	return strings.Join(out, sep)
}
