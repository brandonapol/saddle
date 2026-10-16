package forge

import "testing"

const (
	ghPR = "https://github.com/o/r/pull/12"
	glMR = "https://gitlab.com/g/sub/r/-/merge_requests/12"
)

func TestRefNumber(t *testing.T) {
	for ref, want := range map[string]string{
		ghPR:                     "12",
		ghPR + "/files":          "12",
		glMR:                     "12",
		glMR + "/diffs":          "12",
		glMR + "#note_5":         "12",
		"!12":                    "12",
		"#12":                    "12",
		" 12 ":                   "12",
		"t12":                    "",
		"saddle/t1-alpha":        "",
		"https://gitlab.com/g/r": "",
		"":                       "",
	} {
		if got := RefNumber(ref); got != want {
			t.Errorf("RefNumber(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestPRShort(t *testing.T) {
	for url, want := range map[string]string{
		ghPR:            "#12",
		glMR:            "!12",
		glMR + "/diffs": "!12",
		"t1":            "t1",
	} {
		if got := PRShort(url); got != want {
			t.Errorf("PRShort(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestSamePR(t *testing.T) {
	for _, c := range []struct {
		url, ref string
		want     bool
	}{
		{ghPR, ghPR, true},
		{ghPR, "#12", true},
		{ghPR, "12", true},
		{ghPR, "!12", false}, // "!" is GitLab's
		{ghPR, "#1", false},
		{ghPR, ghPR + "/files", true},
		{glMR, "!12", true},
		{glMR, "#12", true},
		{glMR, "12", true},
		{glMR, "!120", false},
		{glMR, glMR + "/diffs", true},
		{glMR, "http://gitlab.com/g/sub/r/-/merge_requests/12", true},
		{glMR, "https://gitlab.com/g/sub/r/-/merge_requests/13", false},
		{glMR, "https://gitlab.com/g/other/-/merge_requests/12", false},
		{glMR, "t1", false},
		{"", "12", false},
	} {
		if got := SamePR(c.url, c.ref); got != c.want {
			t.Errorf("SamePR(%q, %q) = %v, want %v", c.url, c.ref, got, c.want)
		}
	}
}

func TestRunID(t *testing.T) {
	for link, want := range map[string]string{
		"https://github.com/o/r/actions/runs/123/job/456": "123",
		"https://gitlab.com/g/r/-/pipelines/77":           "77",
		"https://gitlab.com/g/r/-/jobs/9":                 "jobs/9", // a job whose pipeline isn't known
		"https://ci.example.com/build/5":                  "",
	} {
		if got := RunID(link); got != want {
			t.Errorf("RunID(%q) = %q, want %q", link, got, want)
		}
	}
}
