package config

import (
	"bytes"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// SetKey returns content, a config.toml, with the dotted key set to value
// (#908). value is a TOML value ("true", "5", `["claude", "codex"]`,
// `"x"`); one that doesn't parse as TOML is taken as a string, so
// `make lint` needs no quotes. Only the key's line (or lines, for a
// multi-line array) changes: comments and the rest of the file stay. A key
// under a table the file has goes in that table, as a dotted key if need be
// ("train.lint.cmd" is lint.cmd under [train]); otherwise its table is
// added at the end.
func SetKey(content, key, value string) (string, error) {
	parts := strings.Split(key, ".")
	for _, p := range parts {
		if !bareKey.MatchString(p) {
			return "", fmt.Errorf("config key %q: want dotted names of letters, digits, _ and -, e.g. adapters.grok.disabled", key)
		}
	}
	val, err := tomlValue(value)
	if err != nil {
		return "", err
	}
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	// The table to put the key in: the longest prefix of parts the file has
	// a header for, the root table when none.
	headers := tableHeaders(lines)
	table, start, end := 0, 0, len(lines)
	if len(headers) > 0 {
		end = headers[0].line
	}
	for _, h := range headers {
		if len(h.name) < len(parts) && len(h.name) > table && slices.Equal(h.name, parts[:len(h.name)]) {
			table, start, end = len(h.name), h.line+1, h.end
		}
	}
	if table == 0 && len(parts) > 1 {
		// No table for it: add [a.b] with the leaf key, at the end.
		block := []string{"[" + strings.Join(parts[:len(parts)-1], ".") + "]", parts[len(parts)-1] + " = " + val}
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			block = append([]string{""}, block...)
		}
		return strings.Join(append(lines, block...), "\n") + "\n", nil
	}
	sub := strings.Join(parts[table:], ".")
	line := sub + " = " + val
	if len(parts)-table > 1 && !hasDotted(lines[start:end], parts[table]) && !hasAssign(lines[start:end], sub) {
		// adapters.grok.disabled with only [adapters] in the file: give it
		// its own [adapters.grok], as the template writes it, unless the
		// table already uses dotted keys for it (lint.cmd under [train]).
		block := []string{"[" + strings.Join(parts[:len(parts)-1], ".") + "]", parts[len(parts)-1] + " = " + val}
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			block = append([]string{""}, block...)
		}
		return strings.Join(append(lines, block...), "\n") + "\n", nil
	}
	assign := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(sub) + `\s*=`)
	for i := start; i < end; i++ {
		if !assign.MatchString(lines[i]) {
			continue
		}
		j := valueEnd(lines, i, end)
		out := append(slices.Clone(lines[:i]), line)
		return strings.Join(append(out, lines[j+1:]...), "\n") + "\n", nil
	}
	// Not set yet: after the table's last non-blank line.
	at := end
	for at > start && strings.TrimSpace(lines[at-1]) == "" {
		at--
	}
	out := append(slices.Clone(lines[:at]), line)
	return strings.Join(append(out, lines[at:]...), "\n") + "\n", nil
}

type tableHeader struct {
	name []string
	line int // the header's line
	end  int // where its table ends: the next header, or the end
}

var headerLine = regexp.MustCompile(`^\s*\[\s*([A-Za-z0-9_.\- ]+?)\s*\]\s*(#.*)?$`)

// tableHeaders lists the [table] headers in lines ([[arrays]] end the table
// before them but take no keys).
func tableHeaders(lines []string) []tableHeader {
	var hs []tableHeader
	var starts []int
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "[") {
			continue
		}
		starts = append(starts, i)
		if strings.HasPrefix(t, "[[") {
			continue
		}
		if m := headerLine.FindStringSubmatch(l); m != nil {
			var name []string
			for _, p := range strings.Split(m[1], ".") {
				name = append(name, strings.TrimSpace(p))
			}
			hs = append(hs, tableHeader{name: name, line: i})
		}
	}
	for k := range hs {
		hs[k].end = len(lines)
		for _, s := range starts {
			if s > hs[k].line {
				hs[k].end = s
				break
			}
		}
	}
	return hs
}

// valueEnd is the last line of the value assigned on line i: i itself, or
// the line closing a multi-line array or inline table.
func valueEnd(lines []string, i, end int) int {
	depth := 0
	for j := i; j < end; j++ {
		depth += bracketDepth(lines[j])
		if depth <= 0 {
			return j
		}
	}
	return i
}

// bracketDepth is how many more [ and { than ] and } line opens, outside
// strings and comments.
func bracketDepth(line string) int {
	d := 0
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			return d
		case c == '[' || c == '{':
			d++
		case c == ']' || c == '}':
			d--
		}
	}
	return d
}

// tomlValue is value as TOML: itself when it parses as a TOML value, else
// value as a TOML string.
func tomlValue(value string) (string, error) {
	var m map[string]any
	if _, err := toml.Decode("v = "+value, &m); err == nil && !strings.Contains(value, "\n") {
		return strings.TrimSpace(value), nil
	}
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(map[string]string{"v": value}); err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.TrimPrefix(b.String(), "v = ")), nil
}

// UnknownKey reports whether content sets key but saddle reads no such
// setting, e.g. a misspelt "adapters.grok.disabeld". A content that doesn't
// decode says nothing here; Load reports it.
func UnknownKey(content, key string) bool {
	if rest, ok := strings.CutPrefix(key, "adapters."); ok {
		// AdapterMap decodes [adapters] itself, so the decoder's undecoded
		// list says nothing there.
		name, field, sub := strings.Cut(rest, ".")
		if !sub {
			return !slices.Contains(adapterKeys, name) && !slices.Contains(AdapterNames(), name)
		}
		if !slices.Contains(AdapterNames(), name) {
			return true
		}
		f, _, _ := strings.Cut(field, ".")
		return !slices.Contains(adapterFields(), f)
	}
	var c Config
	md, err := toml.Decode(content, &c)
	if err != nil {
		return false
	}
	for _, u := range md.Undecoded() {
		if s := u.String(); s == key || strings.HasPrefix(key, s+".") || strings.HasPrefix(s, key+".") {
			return true
		}
	}
	return false
}

// adapterFields are the keys an [adapters.<name>] table takes.
func adapterFields() []string {
	var fs []string
	t := reflect.TypeFor[Adapter]()
	for i := range t.NumField() {
		if tag, _, _ := strings.Cut(t.Field(i).Tag.Get("toml"), ","); tag != "" && tag != "-" {
			fs = append(fs, tag)
		}
	}
	return fs
}

// hasDotted reports whether lines assign a dotted key under first
// ("first.x = ...").
func hasDotted(lines []string, first string) bool {
	re := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(first) + `\s*\.`)
	return slices.ContainsFunc(lines, re.MatchString)
}

func hasAssign(lines []string, key string) bool {
	re := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(key) + `\s*=`)
	return slices.ContainsFunc(lines, re.MatchString)
}
