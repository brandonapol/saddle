package app

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeTicketGH puts a gh on PATH that answers issue view and the sub-issues
// API, logging each call with its working directory.
func fakeTicketGH(t *testing.T) func() []string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "gh.log")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$1 $2" in
"issue view") echo '{"number":12,"title":"Epic","state":"OPEN","body":"Do it all.","url":"https://github.com/o/r/issues/12","labels":[{"name":"epic"}]}' ;;
"api "*) echo '[{"number":13,"title":"Part one","state":"OPEN","body":"First."},{"number":14,"title":"Part two","state":"CLOSED","body":""}]' ;;
esac
`
	must(t, os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		b, err := os.ReadFile(log)
		must(t, err)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

func TestTicketFetchesSubIssueBodies(t *testing.T) {
	a, _ := setup(t)
	calls := fakeTicketGH(t)
	got, err := a.Ticket(12)
	must(t, err)
	want := Ticket{
		Number: 12, Title: "Epic", State: "OPEN", Body: "Do it all.", Labels: []string{"epic"},
		URL: "https://github.com/o/r/issues/12",
		SubIssues: []SubTicket{
			{Number: 13, Title: "Part one", State: "OPEN", Body: "First."},
			{Number: 14, Title: "Part two", State: "CLOSED"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Ticket:\n got %+v\nwant %+v", got, want)
	}
	c := calls()
	if len(c) != 2 || strings.Contains(c[0], "-R") || !strings.Contains(c[1], "repos/{owner}/{repo}/issues/12/sub_issues") {
		t.Errorf("gh calls = %q", c)
	}
}

func TestTicketInOtherRepo(t *testing.T) {
	a, _ := setup(t)
	calls := fakeTicketGH(t)
	got, err := a.TicketIn("acme/widgets", 12)
	must(t, err)
	if got.Number != 12 || len(got.SubIssues) != 2 {
		t.Fatalf("ticket = %+v", got)
	}
	c := calls()
	if len(c) != 2 || !strings.Contains(c[0], "issue view 12 -R acme/widgets") || !strings.Contains(c[1], "repos/acme/widgets/issues/12/sub_issues") {
		t.Errorf("gh calls = %q", c)
	}
}
