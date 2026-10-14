package hook

import (
	"path/filepath"
	"slices"
	"strings"
)

// The SRE guard's command classifier (#550). It is an allowlist: a command
// is read-only only when every simple command in the line is a program and
// verb known to only read, and nothing writes a file. Anything it can't see
// through (substitutions, an unknown program, an unknown verb, a shell
// wrapper, xargs) is mutating.

// ReadOnlyCommand reports whether a shell line only reads. extra are argvs
// the owner declared read-only (the sre.toml checks), matched word for
// word. When it is not, why names the first part that may mutate.
func ReadOnlyCommand(line string, extra [][]string) (ok bool, why string) {
	cmds, parsed := parseShell(line)
	if !parsed {
		return false, "a substitution or unterminated quote the guard can't see through"
	}
	for _, c := range cmds {
		for _, r := range c.redirs {
			if !inputRedir(r) && !harmless(r) {
				return false, "a redirection that writes " + r.target
			}
		}
		w := c.words[assignments(c.words):]
		if len(w) == 0 {
			continue
		}
		if !readOnlyArgv(w, extra) {
			return false, strings.Join(w, " ")
		}
	}
	return true, ""
}

// inputRedir reads a file, a heredoc or a here-string into the command.
func inputRedir(r redir) bool {
	op := strings.TrimLeft(r.op, "0123456789")
	return op == "<" || op == "<<" || op == "<<<" || op == "<<-"
}

// readOnlyAnyArgs only read or print whatever their arguments.
var readOnlyAnyArgs = map[string]bool{
	"cat": true, "less": true, "more": true, "head": true, "tail": true, "grep": true, "egrep": true,
	"fgrep": true, "rg": true, "ls": true, "pwd": true, "cd": true, "echo": true, "printf": true,
	"wc": true, "sort": true, "uniq": true, "cut": true, "tr": true, "jq": true, "date": true,
	"whoami": true, "id": true, "hostname": true, "uname": true, "df": true, "du": true,
	"free": true, "uptime": true, "ps": true, "which": true, "file": true, "stat": true,
	"diff": true, "cmp": true, "true": true, "false": true, "test": true, "[": true,
	"nslookup": true, "dig": true, "host": true, "ping": true, "traceroute": true,
	"column": true, "nl": true, "base64": true, "sha256sum": true, "md5sum": true, "sleep": true,
}

func readOnlyArgv(w []string, extra [][]string) bool {
	if slices.ContainsFunc(extra, func(e []string) bool { return slices.Equal(e, w) }) {
		return true
	}
	prog, args := filepath.Base(w[0]), w[1:]
	if prog == "time" && len(args) > 0 {
		return readOnlyArgv(args, extra)
	}
	if readOnlyAnyArgs[prog] {
		return true
	}
	switch prog {
	case "find":
		return !slices.ContainsFunc(args, func(a string) bool {
			return a == "-delete" || strings.HasPrefix(a, "-exec") || strings.HasPrefix(a, "-ok") || strings.HasPrefix(a, "-fprint") || a == "-fls"
		})
	case "kubectl":
		return kubectlReadOnly(args)
	case "helm":
		p := positionals(args, helmValued)
		return len(p) > 0 && helmRead[p[0]]
	case "docker":
		p := positionals(args, dockerValued)
		return len(p) > 0 && dockerRead[p[0]]
	case "aws":
		return awsReadOnly(args)
	case "curl":
		return curlReadOnly(args)
	case "git":
		return gitReadOnly(args)
	case "go":
		p := positionals(args, nil)
		return len(p) > 0 && goCode[p[0]] && !(p[0] == "env" && slices.Contains(args, "-w"))
	case "saddle":
		// saddle is the investigators' own coordination surface; only
		// opening or closing the incident is the owner's.
		p := positionals(args, nil)
		return !(len(p) >= 2 && p[0] == "sre" && (p[1] == "open" || p[1] == "close"))
	}
	return false
}

// positionals are args minus flags and the values of flags in valued.
// Everything after "--" is positional.
func positionals(args []string, valued map[string]bool) []string {
	var p []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(p, args[i+1:]...)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			if valued[a] {
				i++
			}
		default:
			p = append(p, a)
		}
	}
	return p
}

var kubectlValued = map[string]bool{
	"-n": true, "--namespace": true, "--context": true, "--kubeconfig": true, "--cluster": true,
	"--user": true, "-s": true, "--server": true, "--as": true, "--as-group": true, "-l": true,
	"--selector": true, "-o": true, "--output": true, "-c": true, "--container": true,
	"--request-timeout": true, "--field-selector": true, "--tail": true, "--since": true,
}

var kubectlRead = map[string]bool{
	"get": true, "describe": true, "logs": true, "top": true, "explain": true, "version": true,
	"api-resources": true, "api-versions": true, "cluster-info": true, "events": true,
}

var kubectlReadSub = map[string]map[string]bool{
	"rollout": {"status": true, "history": true},
	"config":  {"view": true, "get-contexts": true, "current-context": true, "get-clusters": true},
	"auth":    {"can-i": true, "whoami": true},
}

func kubectlReadOnly(args []string) bool {
	p := positionals(args, kubectlValued)
	if len(p) == 0 {
		return false
	}
	if kubectlRead[p[0]] {
		return true
	}
	return len(p) > 1 && kubectlReadSub[p[0]][p[1]]
}

var helmValued = map[string]bool{"--kube-context": true, "-n": true, "--namespace": true, "--kubeconfig": true, "-o": true, "--output": true}

var helmRead = map[string]bool{
	"list": true, "ls": true, "status": true, "history": true, "get": true, "show": true,
	"template": true, "version": true, "search": true, "env": true, "lint": true,
}

var dockerValued = map[string]bool{"-H": true, "--host": true, "--context": true, "--config": true, "-l": true, "--log-level": true}

var dockerRead = map[string]bool{
	"ps": true, "logs": true, "inspect": true, "images": true, "version": true, "info": true,
	"top": true, "stats": true, "port": true, "history": true, "events": true,
}

var awsValued = map[string]bool{
	"--region": true, "--profile": true, "--output": true, "--query": true, "--endpoint-url": true,
	"--cli-read-timeout": true, "--cli-connect-timeout": true, "--color": true,
}

// awsReadOnly takes `aws <service> <operation>` whose operation only
// describes, lists or gets, plus `logs tail` and `s3 ls`.
func awsReadOnly(args []string) bool {
	p := positionals(args, awsValued)
	if len(p) < 2 {
		return false
	}
	svc, op := p[0], p[1]
	switch {
	case svc == "logs" && (op == "tail" || strings.HasPrefix(op, "filter-")),
		svc == "s3" && op == "ls":
		return true
	}
	return strings.HasPrefix(op, "describe-") || strings.HasPrefix(op, "list-") || strings.HasPrefix(op, "get-")
}

// curlWrites are curl's long options that send a body, write a local file
// or read options from one.
var curlWrites = []string{
	"--data", "--form", "--upload-file", "--output", "--remote-name", "--json", "--config",
	"--cookie-jar", "--dump-header", "--trace", "--libcurl", "--etag-save", "--hsts", "--alt-svc",
}

// curlReadOnly takes a curl that only GETs or HEADs and prints the answer.
func curlReadOnly(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return true
		case a == "--request" || strings.HasPrefix(a, "--request="):
			m, ok := strings.CutPrefix(a, "--request=")
			if !ok && i+1 < len(args) {
				i++
				m = args[i]
			}
			if !safeMethod(m) {
				return false
			}
		case strings.HasPrefix(a, "--"):
			if slices.ContainsFunc(curlWrites, func(w string) bool { return strings.HasPrefix(a, w) }) {
				return false
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			// A cluster of short options: -sSfL, -XPOST, -sX PUT.
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'd', 'D', 'F', 'T', 'K', 'o', 'O', 'c':
					return false
				case 'X':
					m := a[j+1:]
					if m == "" && i+1 < len(args) {
						i++
						m = args[i]
					}
					if !safeMethod(m) {
						return false
					}
					j = len(a)
				}
			}
		}
	}
	return true
}

func safeMethod(m string) bool {
	m = strings.ToUpper(m)
	return m == "GET" || m == "HEAD"
}

var gitValued = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true}

// gitRead are git commands that only read the repo. add and commit write
// only the task's own worktree, the code surface claims already guard.
var gitRead = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "blame": true, "grep": true,
	"rev-parse": true, "ls-files": true, "ls-tree": true, "cat-file": true, "describe": true,
	"shortlog": true, "rev-list": true, "merge-base": true, "show-ref": true,
	"add": true, "commit": true,
}

func gitReadOnly(args []string) bool {
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		if gitValued[args[i]] {
			i++
		}
	}
	if i >= len(args) || !gitRead[args[i]] {
		return false
	}
	return !slices.ContainsFunc(args[i+1:], func(a string) bool { return strings.HasPrefix(a, "--output") })
}

// goCode are go commands on the task's own code.
var goCode = map[string]bool{"vet": true, "build": true, "test": true, "list": true, "env": true, "version": true, "doc": true}

// readOnlyTools are tools that only read, or write the task's own worktree,
// which the write guard already holds to its claims.
var readOnlyTools = map[string]bool{
	"Read": true, "Grep": true, "Glob": true, "LS": true, "WebFetch": true, "WebSearch": true,
	"Edit": true, "MultiEdit": true, "Write": true, "NotebookEdit": true, "TodoWrite": true,
	"Task": true, "Agent": true, "ToolSearch": true, "Skill": true,
	// Gemini, Grok and opencode names for the same tools.
	"read_file": true, "read_many_files": true, "list_directory": true, "glob": true, "grep": true,
	"search_file_content": true, "view_file": true, "web_fetch": true, "google_web_search": true,
	"write_file": true, "replace": true, "edit_file": true, "read": true, "list": true, "edit": true,
	"write": true, "multiedit": true, "patch": true, "apply_patch": true, "webfetch": true, "todowrite": true,
}

// readOnlyTool classifies a tool call: shell tools by their command, MCP
// tools as mutating unless they are saddle's own, unknown tools as mutating.
func readOnlyTool(tool string, input map[string]any, extra [][]string) (bool, string) {
	if cmd, _ := input["command"].(string); cmd != "" {
		return ReadOnlyCommand(cmd, extra)
	}
	if readOnlyTools[tool] {
		return true, ""
	}
	if strings.HasPrefix(tool, "mcp__saddle__") || strings.HasPrefix(tool, "mcp__plugin_saddle_") {
		return true, ""
	}
	return false, "tool " + tool
}
