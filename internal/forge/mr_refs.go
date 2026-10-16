package forge

import (
	"regexp"
	"strings"
)

// Saddle stores a PR's URL on its task and lets people name it by URL or
// number. These helpers read either provider's form, so no caller parses
// GitHub's /pull/N itself: a GitHub PR is …/pull/12 and "#12"; a GitLab MR
// is …/-/merge_requests/12 (with any page after it: /diffs, #note_5) and
// "!12". A bare "12" or "#12" names either.

var (
	prURLNumRe  = regexp.MustCompile(`^(\S*?/pull/\d+)(?:[/?#].*)?$`)
	mrURLNumRe  = regexp.MustCompile(`^(\S*?/-/merge_requests/\d+)(?:[/?#].*)?$`)
	refNumberRe = regexp.MustCompile(`^[#!]?(\d+)$`)
	runIDRe     = regexp.MustCompile(`/(?:actions/runs|-/pipelines)/(\d+)`)
)

// canonicalPR is url up to its PR or MR number, and whether it is an MR.
// ok is false when url is neither.
func canonicalPR(url string) (base string, mr, ok bool) {
	url = strings.TrimSpace(url)
	if m := mrURLNumRe.FindStringSubmatch(url); m != nil {
		return m[1], true, true
	}
	if m := prURLNumRe.FindStringSubmatch(url); m != nil {
		return m[1], false, true
	}
	return "", false, false
}

// RefNumber is the number of the PR or MR ref names: a PR or MR URL, "#12",
// "!12" or "12". It is "" for anything else.
func RefNumber(ref string) string {
	ref = strings.TrimSpace(ref)
	if base, _, ok := canonicalPR(ref); ok {
		return base[strings.LastIndexByte(base, '/')+1:]
	}
	if m := refNumberRe.FindStringSubmatch(ref); m != nil {
		return m[1]
	}
	return ""
}

// PRShort is the short name of the PR at url: "#12" for a GitHub PR, "!12"
// for a GitLab MR, url itself when it is neither.
func PRShort(url string) string {
	base, mr, ok := canonicalPR(url)
	if !ok {
		return url
	}
	n := base[strings.LastIndexByte(base, '/')+1:]
	if mr {
		return "!" + n
	}
	return "#" + n
}

// SamePR reports whether ref names the PR or MR at url: the URL, the URL
// with a page after its number, or its number as "12" or "#12" ("!12" only
// for an MR).
func SamePR(url, ref string) bool {
	url, ref = strings.TrimSpace(url), strings.TrimSpace(ref)
	if url == "" || ref == "" {
		return false
	}
	if url == ref {
		return true
	}
	base, mr, ok := canonicalPR(url)
	if !ok {
		return false
	}
	if rbase, _, rok := canonicalPR(ref); rok {
		return noScheme(rbase) == noScheme(base)
	}
	m := refNumberRe.FindStringSubmatch(ref)
	if m == nil || strings.HasPrefix(ref, "!") && !mr {
		return false
	}
	return base[strings.LastIndexByte(base, '/')+1:] == m[1]
}

// noScheme is url without http:// or https://.
func noScheme(url string) string {
	return strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
}

// RunID is the CI run a check's run link names, for RerunFailed: a GitHub
// Actions run (…/actions/runs/N) or a GitLab pipeline (…/-/pipelines/N).
// It is "" for any other link, such as a commit status's page.
func RunID(link string) string {
	if m := runIDRe.FindStringSubmatch(link); m != nil {
		return m[1]
	}
	return ""
}
