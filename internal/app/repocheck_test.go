package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRepoGH answers `gh api repos/{owner}/{repo}` with the given merge
// settings and records the args it was called with.
func fakeRepoGH(merge, squash, rebase bool) (RepoGH, *[]string) {
	var got []string
	return func(args ...string) (string, error) {
		got = args
		return fmt.Sprintf(`{"full_name":"acme/widgets","html_url":"https://github.com/acme/widgets",`+
			`"allow_merge_commit":%t,"allow_squash_merge":%t,"allow_rebase_merge":%t}`, merge, squash, rebase), nil
	}, &got
}

func TestCheckRepoMergeSettingsCombinations(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		merge, squash, rebase bool
		refuse                bool
	}{
		{false, true, false, false},
		{false, false, true, false}, // rebase-merge keeps history linear
		{false, true, true, false},
		{true, false, false, true},
		{true, true, false, true},
		{true, false, true, true},
		{true, true, true, true},
	} {
		name := fmt.Sprintf("merge=%t,squash=%t,rebase=%t", c.merge, c.squash, c.rebase)
		t.Run(name, func(t *testing.T) {
			gh, got := fakeRepoGH(c.merge, c.squash, c.rebase)
			var warn bytes.Buffer
			err := CheckRepoMergeSettings(gh, &warn)
			if strings.Join(*got, " ") != "api repos/{owner}/{repo}" {
				t.Errorf("gh called with %q", *got)
			}
			if c.refuse != errors.Is(err, ErrMergeCommitsAllowed) {
				t.Fatalf("refuse=%t, got err %v", c.refuse, err)
			}
			if !c.refuse && err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if warn.Len() != 0 {
				t.Errorf("unexpected warning %q", warn.String())
			}
		})
	}
}

func TestCheckRepoMergeSettingsRefusalMessage(t *testing.T) {
	t.Parallel()
	gh, _ := fakeRepoGH(true, true, false)
	err := CheckRepoMergeSettings(gh, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected refusal")
	}
	msg := err.Error()
	for _, want := range []string{
		"acme/widgets allows merge commits",
		"https://github.com/acme/widgets/settings",
		"Settings -> General -> Pull Requests",
		"gh api -X PATCH repos/acme/widgets -F allow_merge_commit=false",
		"Saddle will not change this setting for you",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message missing %q:\n%s", want, msg)
		}
	}
}

func TestCheckRepoMergeSettingsUnreadableWarns(t *testing.T) {
	t.Parallel()
	for name, gh := range map[string]RepoGH{
		"gh fails": func(...string) (string, error) {
			return "", errors.New("gh api repos/{owner}/{repo}: exit status 1: gh auth login required")
		},
		"not json": func(...string) (string, error) { return "garbage", nil },
		// GitHub omits the allow_* fields for users without admin/push rights.
		"fields missing": func(...string) (string, error) {
			return `{"full_name":"acme/widgets"}`, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			var warn bytes.Buffer
			if err := CheckRepoMergeSettings(gh, &warn); err != nil {
				t.Fatalf("unreadable settings must not block, got %v", err)
			}
			w := warn.String()
			if !strings.HasPrefix(w, "warning: could not read the repo's merge settings") {
				t.Errorf("warning = %q", w)
			}
			if !strings.Contains(w, "allow_merge_commit") {
				t.Errorf("warning should say what to check by hand: %q", w)
			}
		})
	}
}

// TestAppCheckMergeSettingsRunsGH checks the App method calls the real gh
// binary in the repo root.
func TestAppCheckMergeSettingsRunsGH(t *testing.T) {
	a, _ := setup(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "gh.log")
	script := "#!/bin/sh\necho \"$PWD $*\" >> " + log + "\n" +
		`echo '{"full_name":"acme/widgets","allow_merge_commit":true,"allow_squash_merge":true,"allow_rebase_merge":false}'` + "\n"
	must(t, os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := a.CheckMergeSettings(&bytes.Buffer{}); !errors.Is(err, ErrMergeCommitsAllowed) {
		t.Fatalf("want ErrMergeCommitsAllowed, got %v", err)
	}
	b, err := os.ReadFile(log)
	must(t, err)
	if got := strings.TrimSpace(string(b)); got != a.Root+" api repos/{owner}/{repo}" {
		t.Errorf("gh call = %q", got)
	}
}
