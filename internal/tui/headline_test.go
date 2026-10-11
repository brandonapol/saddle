package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"time"

	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
)

// The header's golden shape (#403): the headline owns the top line and the
// counters drop to a second line, at 80 and 200 columns.
func TestHeaderHeadlineGolden(t *testing.T) {
	cases := []struct {
		width int
		want  string
	}{
		{80, " SADDLE   ▲ Needs you: t5 is waiting at a prompt.\n" +
			" 1 control  2 plan  3 merge  4 runs  ▲ 1 need you  ● 1 running        orch idle "},
		{200, " SADDLE   ▲ Needs you: t5 is waiting at a prompt.\n" +
			" 1 control  2 plan  3 merge  4 runs  ▲ 1 need you  ● 1 running  ◆ train 1 queued  ✓ 1 landed  saddle-repo  main → saddle/integration                                   orchestrator opus · idle · $0.00 "},
	}
	for _, c := range cases {
		m := newViewModel(c.width, 30)
		m.tasks = []mcpserver.TaskView{
			{ID: "t1", Status: store.Running},
			{ID: "t2", Status: store.Done, Train: "queued"},
			{ID: "t4", Status: store.Landed, Train: "landed"},
			{ID: "t5", Status: store.NeedsYou},
		}
		h := m.viewHeader()
		if h != c.want {
			t.Errorf("width %d header:\n%q\nwant\n%q", c.width, h, c.want)
		}
		if lipgloss.Width(h) > c.width {
			t.Errorf("width %d: header is %d wide", c.width, lipgloss.Width(h))
		}
	}
}

func TestRenderHeadlineClipsToWidth(t *testing.T) {
	h := app.StateHeadline{Class: app.HeadlineWorking, Color: app.ColorGreen, Text: "Working: 3 agents running, 2 PRs merging. Nothing needs you."}
	for _, w := range []int{8, 20, 40, 80} {
		got := renderHeadline(h, "", w)
		if lipgloss.Width(got) > w {
			t.Errorf("width %d: %q is %d wide", w, got, lipgloss.Width(got))
		}
	}
	if got := renderHeadline(h, "", 200); got != " SADDLE   ● "+h.Text {
		t.Errorf("wide headline = %q", got)
	}
}

func TestHeadlineFromModelWithoutStore(t *testing.T) {
	m := newViewModel(80, 30)
	m.tasks = []mcpserver.TaskView{{ID: "t1", Status: store.Running}}
	if h := m.headline(time.Now()); h.Class != app.HeadlineWorking || h.Color != app.ColorGreen {
		t.Fatalf("headline = %+v", h)
	}
}

// The repo name sits in its own pill next to SADDLE (#414): whole when it
// fits, clipped with an ellipsis when it does not, never past the width.
func TestHeaderRepoPill(t *testing.T) {
	for _, c := range []struct {
		width int
		repo  string
		want  string
	}{
		{80, "saddle", " SADDLE   saddle   ▲ Needs you: t5 is waiting at a prompt."},
		{200, "saddle", " SADDLE   saddle   ▲ Needs you: t5 is waiting at a prompt."},
		{80, strings.Repeat("longrepo", 10), " SADDLE   longrepolongrepolongrep…   ▲ Needs you: t5 is waiting at a prompt."},
		{20, strings.Repeat("longrepo", 10), " SADDLE   lon… "},
		{12, "saddle", " SADDLE "},
	} {
		m := newViewModel(c.width, 30)
		m.app.Root = "/work/" + c.repo
		m.tasks = []mcpserver.TaskView{{ID: "t5", Status: store.NeedsYou}}
		top := strings.Split(m.viewHeader(), "\n")[0]
		if got := ansi.Strip(top); got != c.want {
			t.Errorf("width %d header top %q, want %q", c.width, got, c.want)
		}
		if w := lipgloss.Width(top); w > c.width {
			t.Errorf("width %d: top line is %d wide", c.width, w)
		}
	}
}
