package hook

import "strings"

// simpleCmd is one simple command of a shell line: its words with quotes
// removed, its redirections, and the operator that ends it ("" at the end).
type simpleCmd struct {
	words  []string
	redirs []redir
	sep    string
}

// redir is one redirection: its operator with any fd number ("2>&", ">>")
// and its target word.
type redir struct{ op, target string }

// parseShell splits a shell line into simple commands, honoring quotes,
// backslashes and comments. It knows the list and pipe operators (; & &&
// | || |& newline), parentheses and redirections, nothing else: no
// expansion, keywords or heredoc bodies. ok is false when the line has
// something it can't see through: command or process substitution, or an
// unterminated quote.
func parseShell(line string) (cmds []simpleCmd, ok bool) {
	var (
		cur        simpleCmd
		word       strings.Builder
		inWord     bool
		quoted     bool // the current word had quotes or escapes
		sq, dq, bs bool
		pending    *redir // a redirection waiting for its target
	)
	ok = true
	endWord := func() {
		if !inWord {
			return
		}
		w := word.String()
		word.Reset()
		inWord, quoted = false, false
		if pending != nil {
			pending.target = w
			cur.redirs = append(cur.redirs, *pending)
			pending = nil
			return
		}
		cur.words = append(cur.words, w)
	}
	endCmd := func(sep string) {
		endWord()
		if pending != nil { // a redirection without a target
			cur.redirs = append(cur.redirs, *pending)
			pending = nil
		}
		// An empty command is kept when its operator matters: "(", or
		// "&&" after nothing.
		if len(cur.words) > 0 || len(cur.redirs) > 0 || (sep != "" && sep != ";" && sep != "\n") {
			cur.sep = sep
			cmds = append(cmds, cur)
		}
		cur = simpleCmd{}
	}
	rs := []rune(line)
	at := func(i int) rune {
		if i < len(rs) {
			return rs[i]
		}
		return 0
	}
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case bs:
			if r != '\n' { // backslash-newline continues the line
				word.WriteRune(r)
			}
			bs = false
		case sq:
			if r == '\'' {
				sq = false
			} else {
				word.WriteRune(r)
			}
		case dq:
			switch r {
			case '"':
				dq = false
			case '\\':
				bs = true
			case '`':
				ok = false
				word.WriteRune(r)
			case '$':
				if at(i+1) == '(' {
					ok = false
				}
				word.WriteRune(r)
			default:
				word.WriteRune(r)
			}
		case r == '\\':
			bs, inWord, quoted = true, true, true
		case r == '\'':
			sq, inWord, quoted = true, true, true
		case r == '"':
			dq, inWord, quoted = true, true, true
		case r == '`':
			ok = false
			word.WriteRune(r)
			inWord = true
		case r == '$' && at(i+1) == '(':
			ok = false
			word.WriteRune(r)
			inWord = true
		case r == '#' && !inWord:
			for i+1 < len(rs) && rs[i+1] != '\n' {
				i++
			}
		case r == ' ' || r == '\t':
			endWord()
		case r == ';' || r == '\n':
			endCmd(string(r))
		case r == '(' || r == ')':
			if inWord && (word.String() == "<" || word.String() == ">") {
				ok = false // process substitution
			}
			endCmd(string(r))
		case r == '&' && at(i+1) == '&':
			endCmd("&&")
			i++
		case r == '|' && at(i+1) == '|':
			endCmd("||")
			i++
		case r == '|' && at(i+1) == '&':
			endCmd("|&")
			i++
		case r == '|':
			endCmd("|")
		case r == '&' && at(i+1) != '>':
			endCmd("&")
		case r == '<' || r == '>' || r == '&':
			if r != '&' && at(i+1) == '(' {
				ok = false // process substitution
			}
			op := ""
			if inWord && !quoted && isDigits(word.String()) {
				op = word.String() // an fd number: 2>
				word.Reset()
				inWord = false
			}
			endWord()
			op += string(r)
			next := func(cs string) bool {
				if c := at(i + 1); c != 0 && strings.ContainsRune(cs, c) {
					i++
					op += string(c)
					return true
				}
				return false
			}
			switch r {
			case '&': // &> &>>
				if next(">") {
					next(">")
				}
			case '>': // >> >& >|
				next(">&|")
			case '<': // << <<< <<- <& <>
				if next("<") {
					next("<-")
				} else {
					next("&>")
				}
			}
			if strings.HasSuffix(op, "&") && next("-") { // >&- closes the fd: no target
				cur.redirs = append(cur.redirs, redir{op: op})
				continue
			}
			pending = &redir{op: op}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if sq || dq || bs {
		ok = false
	}
	endCmd("")
	return cmds, ok
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// segments are a shell line's simple commands' words.
func segments(cmd string) [][]string {
	cmds, _ := parseShell(cmd)
	var out [][]string
	for _, c := range cmds {
		if len(c.words) > 0 {
			out = append(out, c.words)
		}
	}
	return out
}

// assignments counts the leading VAR=value words of a simple command.
func assignments(w []string) int {
	n := 0
	for n < len(w) && isAssignment(w[n]) {
		n++
	}
	return n
}

func isAssignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	if !ok || name == "" {
		return false
	}
	for i, c := range name {
		letter := c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !letter && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}
