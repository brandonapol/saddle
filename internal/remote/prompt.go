package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
)

// Option is one numbered answer a prompt offers. Pressing N picks it.
type Option struct {
	N     int    `json:"n"`
	Label string `json:"label"`
}

// Prompt is what an agent's screen asks: its kind (app.DetectPrompt), the
// question line and the numbered options. Question and options come from
// the agent's screen, so they are scrubbed, clipped and untrusted.
type Prompt struct {
	Kind     string   `json:"kind,omitempty"`
	Question string   `json:"question,omitempty"`
	Options  []Option `json:"options,omitempty"`
	// block is the prompt as shown, from a little above the question to
	// its last option; the item id hashes it.
	block string
}

// Caps on prompt fields.
const (
	maxQuestion = 300
	maxOption   = 200
	// promptContext is how many lines above the question belong to a
	// prompt drawn without a box (the command or diff it asks about).
	promptContext = 12
)

var (
	// optionRe is a numbered option line, maybe behind the selection cursor.
	optionRe = regexp.MustCompile(`^(?:[❯>›▶]\s*)?(\d{1,2})\.\s+(\S.*)$`)
	// boxChars frame Claude Code's dialogs.
	boxChars = " \t│┃|╭╮╰╯─━┌┐└┘"
)

// ParsePrompt reads the prompt an agent's screen shows: its question and
// numbered options. The options are the last run on the screen numbered
// 1, 2, 3…, so a numbered list printed earlier isn't taken for them. An
// option's indented continuation lines join its label.
func ParsePrompt(screen string) Prompt {
	kind := app.DetectPrompt(screen)
	if kind == app.PromptNone {
		return Prompt{}
	}
	raw := strings.Split(stripControl(screen), "\n")
	lines := make([]string, len(raw))
	indent := make([]int, len(raw))
	for i, l := range raw {
		l = strings.TrimLeft(l, "│┃|")
		body := strings.TrimLeft(l, " \t")
		indent[i], lines[i] = len(l)-len(body), strings.Trim(body, boxChars)
	}
	var best, cur []Option
	start, end, curStart, optIndent := -1, -1, -1, 0
	for i, l := range lines {
		if m := optionRe.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[1])
			switch {
			case n == 1:
				cur, curStart = []Option{{N: 1, Label: m[2]}}, i
			case len(cur) > 0 && n == cur[len(cur)-1].N+1:
				cur = append(cur, Option{N: n, Label: m[2]})
			default:
				cur = nil
				continue
			}
			optIndent = indent[i]
			best, start, end = cur, curStart, i
			continue
		}
		if len(cur) > 0 && l != "" && end == i-1 && indent[i] > optIndent+1 {
			cur[len(cur)-1].Label += " " + l // a long option, wrapped
			end = i
			continue
		}
		if l != "" {
			cur = nil
		}
	}
	p := Prompt{Kind: kind}
	if start < 0 {
		return p
	}
	for _, o := range best {
		p.Options = append(p.Options, Option{N: o.N, Label: scrubText(strings.Join(strings.Fields(o.Label), " "), maxOption)})
	}
	q := -1
	for i := start - 1; i >= 0 && i >= start-promptContext; i-- {
		if lines[i] == "" {
			continue
		}
		if q < 0 {
			q = i
		}
		if strings.HasSuffix(lines[i], "?") {
			q = i
			break
		}
	}
	from := max(start-promptContext, 0)
	if q >= 0 {
		p.Question = scrubText(lines[q], maxQuestion)
		from = max(q-promptContext, 0)
	}
	// A dialog's box bounds the prompt; above it, a spinner or timer
	// would change the id while the prompt stays the same.
	for i := start; i >= 0 && i >= start-2*promptContext; i-- {
		if strings.HasPrefix(strings.TrimSpace(raw[i]), "╭") {
			from = i
			break
		}
	}
	p.block = strings.Join(lines[from:end+1], "\n")
	return p
}

// promptID names a prompt item: the task and a hash of the prompt as
// shown. It is stable while the screen shows the same prompt and changes
// when the prompt does, so an answer bound to it can't land on a newer one.
func promptID(task, basis string) string {
	h := sha256.Sum256([]byte(task + "\x00" + basis))
	return "p-" + task + "-" + hex.EncodeToString(h[:6])
}

func noticeID(id int64) string { return "n-" + strconv.FormatInt(id, 10) }
