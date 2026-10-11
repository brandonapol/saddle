package remote

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/tmux"
)

// The remote action tools (#313). Each one names its target explicitly,
// takes the if_version of the object it changes (from status.versions),
// and is idempotent: asking for the state that already holds is a no-op
// success, whatever the version, so a retry is safe. Otherwise a moved
// version refuses the call with the object as it is now. The check runs
// when the call runs, which is after its confirm, and calls on one repo
// run one at a time. The work itself is the orchestrator's own tool,
// called in process, so a remote action does what a local one does.

// ActOut is what an action tool returns.
type ActOut struct {
	Message string `json:"message"`
	Noop    bool   `json:"noop,omitempty" jsonschema:"the state asked for already held; nothing changed"`
	Object  string `json:"object,omitempty" jsonschema:"the versioned object the call is checked against"`
	Version string `json:"version,omitempty" jsonschema:"that object's version now; pass it as if_version next time"`
}

// versioned is an action input with the optimistic check: if_version, the
// object's version from status.versions. (A field per type, since the
// schema doesn't see embedded ones.)
type versioned interface{ ifVersion() string }

func (in QueueMoveIn) ifVersion() string      { return in.IfVersion }
func (in QueueHoldIn) ifVersion() string      { return in.IfVersion }
func (in TaskActIn) ifVersion() string        { return in.IfVersion }
func (in AutomergeActIn) ifVersion() string   { return in.IfVersion }
func (in ConcurrencyActIn) ifVersion() string { return in.IfVersion }
func (in PublishActIn) ifVersion() string     { return in.IfVersion }
func (in StackActIn) ifVersion() string       { return in.IfVersion }

// Action inputs. Every target is explicit.
type (
	AnswerIn struct {
		Item   string `json:"item" jsonschema:"the needs_you item id of the prompt"`
		Option int    `json:"option" jsonschema:"the number of the option to pick"`
	}
	SendKeysIn struct {
		Task string   `json:"task"`
		Text string   `json:"text,omitempty" jsonschema:"text to type, then Enter"`
		Keys []string `json:"keys,omitempty" jsonschema:"keys to press instead, e.g. Escape"`
	}
	MessageIn struct {
		Task    string `json:"task"`
		Message string `json:"message"`
	}
	QueueMoveIn struct {
		Task      string `json:"task"`
		Position  int    `json:"position" jsonschema:"1 lands next"`
		IfVersion string `json:"if_version" jsonschema:"the object's version from status.versions; refused if it moved since"`
	}
	QueueHoldIn struct {
		Task      string `json:"task"`
		Reason    string `json:"reason,omitempty"`
		IfVersion string `json:"if_version" jsonschema:"the object's version from status.versions; refused if it moved since"`
	}
	TaskActIn struct {
		Task      string `json:"task"`
		IfVersion string `json:"if_version" jsonschema:"the object's version from status.versions; refused if it moved since"`
	}
	AutomergeActIn struct {
		Action    string `json:"action" jsonschema:"on, off, hold or release"`
		Stack     string `json:"stack,omitempty" jsonschema:"for hold and release: a stack name, task id or PR"`
		IfVersion string `json:"if_version" jsonschema:"the object's version from status.versions; refused if it moved since"`
	}
	ConcurrencyActIn struct {
		Limit     int    `json:"limit,omitempty"`
		Reset     bool   `json:"reset,omitempty" jsonschema:"drop the runtime override, back to config"`
		IfVersion string `json:"if_version" jsonschema:"the object's version from status.versions; refused if it moved since"`
	}
	PublishActIn struct {
		Target     string `json:"target" jsonschema:"task id"`
		Base       string `json:"base,omitempty"`
		BranchName string `json:"branch_name,omitempty"`
		Draft      bool   `json:"draft,omitempty"`
		IfVersion  string `json:"if_version" jsonschema:"the object's version from status.versions; refused if it moved since"`
	}
	StackActIn struct {
		IfVersion string `json:"if_version" jsonschema:"the object's version from status.versions; refused if it moved since"`
	}
)

// plan is an action's decision against the current state: whether it is
// already done, and the orchestrator tool's arguments if not.
type plan[In any] func(st State, in In) (done bool, args map[string]any, err error)

// actEnv is what the action tools of one served repo share.
type actEnv struct {
	src Source
	act Actor
	mu  *sync.Mutex // one action at a time per repo
	who string      // the caller's token name
}

func needTask(task string) error {
	if strings.TrimSpace(task) == "" {
		return errors.New("name the task: remote actions never act on a current or default target")
	}
	return nil
}

func queueIndex(st State, task string) int {
	return slices.IndexFunc(st.Queue, func(e QueueEntry) bool { return e.Task == task })
}

// addVersioned registers a versioned action tool.
func addVersioned[In versioned](s *mcp.Server, env actEnv, name, desc, obj string, p plan[In]) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc + " Takes if_version: status.versions." + obj + "."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, ActOut, error) {
			out, err := env.runVersioned(ctx, name, obj, in.ifVersion(), func(st State) (bool, map[string]any, error) { return p(st, in) })
			return nil, out, err
		})
}

func (env actEnv) runVersioned(ctx context.Context, name, obj, ifv string, p func(State) (bool, map[string]any, error)) (ActOut, error) {
	env.mu.Lock()
	defer env.mu.Unlock()
	st, err := env.act.State()
	if err != nil {
		return ActOut{}, err
	}
	done, args, err := p(st)
	if err != nil {
		return ActOut{}, err
	}
	cur := st.Versions()[obj]
	if done {
		return ActOut{Message: name + ": already so; nothing changed", Noop: true, Object: obj, Version: cur}, nil
	}
	if ifv == "" {
		return ActOut{}, fmt.Errorf("%s needs if_version: pass status.versions.%s (now %s)", name, obj, cur)
	}
	if ifv != cur {
		return ActOut{}, fmt.Errorf("%s refused: %s moved since you looked (if_version %s, now %s). It is now %s. Check it and call again if it still makes sense",
			name, obj, ifv, cur, st.describe(obj))
	}
	msg, err := env.act.Call(ctx, name, args)
	if err != nil {
		return ActOut{}, err
	}
	out := ActOut{Message: msg, Object: obj}
	if st, err := env.act.State(); err == nil {
		out.Version = st.Versions()[obj]
	}
	return out, nil
}

// actionTools are the act and land tools, served when src can act.
func actionTools(env actEnv) []toolDef {
	return []toolDef{
		{"answer", func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{Name: "answer", Description: "Pick an option at an agent's prompt, by the needs_you item id and option number. " +
				"It presses the option only while that prompt still stands: if the item is gone or changed, nothing is pressed."},
				func(ctx context.Context, _ *mcp.CallToolRequest, in AnswerIn) (*mcp.CallToolResult, ActOut, error) {
					out, err := env.answer(ctx, in)
					return nil, out, err
				})
		}},
		{"send_keys", func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{Name: "send_keys", Description: "Type text (then Enter) or press keys in an agent's terminal. Safe-send: refused while the owner has a draft in that pane, " +
				"and refused at a prompt; answer a prompt with answer {item, option}."},
				func(ctx context.Context, _ *mcp.CallToolRequest, in SendKeysIn) (*mcp.CallToolResult, ActOut, error) {
					out, err := env.sendKeys(ctx, in)
					return nil, out, err
				})
		}},
		{"message", func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{Name: "message", Description: "Send a [saddle] message to a task, signed with this token's name. It wakes the agent if it is idle."},
				func(ctx context.Context, _ *mcp.CallToolRequest, in MessageIn) (*mcp.CallToolResult, ActOut, error) {
					if err := needTask(in.Task); err != nil {
						return nil, ActOut{}, err
					}
					if strings.TrimSpace(in.Message) == "" {
						return nil, ActOut{}, errors.New("message is empty")
					}
					env.mu.Lock()
					defer env.mu.Unlock()
					msg, err := env.act.Call(ctx, "message", map[string]any{"task": in.Task, "message": fmt.Sprintf("(remote, %s) %s", env.who, in.Message)})
					return nil, ActOut{Message: msg}, err
				})
		}},
		{"queue_move", func(s *mcp.Server) {
			addVersioned(s, env, "queue_move", "Move a branch waiting in the merge train to a position (1 lands next).", ObjQueue,
				func(st State, in QueueMoveIn) (bool, map[string]any, error) {
					if err := needTask(in.Task); err != nil {
						return false, nil, err
					}
					if in.Position < 1 {
						return false, nil, errors.New("position starts at 1")
					}
					i := queueIndex(st, in.Task)
					return i >= 0 && i == min(in.Position, len(st.Queue))-1, map[string]any{"task": in.Task, "position": in.Position}, nil
				})
		}},
		{"queue_hold", func(s *mcp.Server) {
			addVersioned(s, env, "queue_hold", "Keep a waiting branch from landing without losing its place. Holding a held branch is a no-op.", ObjQueue,
				func(st State, in QueueHoldIn) (bool, map[string]any, error) {
					if err := needTask(in.Task); err != nil {
						return false, nil, err
					}
					reason := in.Reason
					if reason == "" {
						reason = "held remotely by " + env.who
					}
					i := queueIndex(st, in.Task)
					return i >= 0 && st.Queue[i].Held, map[string]any{"task": in.Task, "reason": reason}, nil
				})
		}},
		{"queue_release", func(s *mcp.Server) {
			addVersioned(s, env, "queue_release", "Let a held branch land again from its place. Releasing a branch that isn't held is a no-op.", ObjQueue,
				func(st State, in TaskActIn) (bool, map[string]any, error) {
					if err := needTask(in.Task); err != nil {
						return false, nil, err
					}
					i := queueIndex(st, in.Task)
					return i >= 0 && !st.Queue[i].Held, map[string]any{"task": in.Task}, nil
				})
		}},
		{"automerge", func(s *mcp.Server) {
			addVersioned(s, env, "automerge", "Turn auto-merge on or off, or hold or release one stack. Setting the state that holds is a no-op; status shows the state.", ObjAutoMerge,
				func(st State, in AutomergeActIn) (bool, map[string]any, error) {
					args := map[string]any{"action": in.Action}
					switch in.Action {
					case "on":
						return st.AutoMerge.On && st.AutoMerge.Stopped == "", args, nil
					case "off":
						return !st.AutoMerge.On, args, nil
					case "hold", "release":
						if strings.TrimSpace(in.Stack) == "" {
							return false, nil, fmt.Errorf("%s needs a stack, task or PR", in.Action)
						}
						args["stack"] = in.Stack
						if in.Action == "hold" {
							return slices.Contains(st.AutoMerge.Holds, in.Stack), args, nil
						}
						return len(st.AutoMerge.Holds) == 0, args, nil
					}
					return false, nil, fmt.Errorf("action %q: want on, off, hold or release (status shows the state)", in.Action)
				})
		}},
		{"concurrency", func(s *mcp.Server) {
			addVersioned(s, env, "concurrency", fmt.Sprintf("Set the bots limit (%d-%d) or reset it to config. Setting the limit in force is a no-op.", app.MinConcurrency, app.MaxConcurrency), ObjConcurrency,
				func(st State, in ConcurrencyActIn) (bool, map[string]any, error) {
					switch {
					case in.Reset && in.Limit != 0:
						return false, nil, errors.New("give limit or reset, not both")
					case in.Reset:
						return !st.Concurrency.Runtime, map[string]any{"reset": true}, nil
					case in.Limit != 0:
						return st.Concurrency.Runtime && st.Concurrency.Limit == in.Limit, map[string]any{"limit": in.Limit}, nil
					}
					return false, nil, errors.New("give limit or reset; status shows the limit in force")
				})
		}},
		{"unstack", func(s *mcp.Server) {
			addVersioned(s, env, "unstack", "Take a landed task out of the PR stack for good. A task already out is a no-op.", ObjStack,
				func(st State, in TaskActIn) (bool, map[string]any, error) {
					if err := needTask(in.Task); err != nil {
						return false, nil, err
					}
					ts := st.Train[in.Task]
					return ts == app.TrainSuperseded || ts == app.TrainMerged, map[string]any{"task": in.Task}, nil
				})
		}},
		{"requeue", func(s *mcp.Server) {
			addVersioned(s, env, "requeue", "Put a landed task whose work is missing from integration back in the merge train. A task already waiting is a no-op.", ObjQueue,
				func(st State, in TaskActIn) (bool, map[string]any, error) {
					if err := needTask(in.Task); err != nil {
						return false, nil, err
					}
					ts := st.Train[in.Task]
					return ts == store.Queued || ts == store.OnHold, map[string]any{"task": in.Task}, nil
				})
		}},
		{"publish", func(s *mcp.Server) {
			addVersioned(s, env, "publish", "Publish one task's own commits as an independent PR against base. An open PR for it is reported, not duplicated.", ObjStack,
				func(_ State, in PublishActIn) (bool, map[string]any, error) {
					if err := needTask(in.Target); err != nil {
						return false, nil, err
					}
					args := map[string]any{"target": in.Target, "draft": in.Draft}
					if in.Base != "" {
						args["base"] = in.Base
					}
					if in.BranchName != "" {
						args["branch_name"] = in.BranchName
					}
					return false, args, nil
				})
		}},
		{"land", func(s *mcp.Server) {
			addVersioned(s, env, "land", "Run the merge train over the queued branches. With nothing queued it is a no-op.", ObjQueue,
				func(st State, _ StackActIn) (bool, map[string]any, error) {
					return !slices.ContainsFunc(st.Queue, func(e QueueEntry) bool { return !e.Held }), map[string]any{}, nil
				})
		}},
		{"prs", func(s *mcp.Server) {
			addVersioned(s, env, "prs", "Push landed branches and open or update the PR stack. Running it twice changes nothing more.", ObjStack,
				func(State, StackActIn) (bool, map[string]any, error) { return false, map[string]any{}, nil })
		}},
		{"restack", func(s *mcp.Server) {
			addVersioned(s, env, "restack", "Rebuild the landed stack on origin's base. A stack already on base moves nothing.", ObjStack,
				func(State, StackActIn) (bool, map[string]any, error) { return false, map[string]any{}, nil })
		}},
		{"sentinel_ack", func(s *mcp.Server) {
			addVersioned(s, env, "sentinel_ack", "Acknowledge the stack's at-risk flag and every ci-red hold. With nothing unacknowledged it is a no-op.", ObjStack,
				func(st State, _ StackActIn) (bool, map[string]any, error) {
					done := st.Stack.Flag == nil || st.Stack.Flag.Acked
					for _, l := range st.Stack.CIRed {
						done = done && l.Acked
					}
					return done, map[string]any{}, nil
				})
		}},
	}
}

// answer presses option at the prompt item names, only while that prompt
// stands: the item id hashes the prompt as shown, so a prompt that changed
// or was answered has no item with that id.
func (env actEnv) answer(ctx context.Context, in AnswerIn) (ActOut, error) {
	env.mu.Lock()
	defer env.mu.Unlock()
	items, err := env.src.NeedsYou()
	if err != nil {
		return ActOut{}, err
	}
	i := slices.IndexFunc(items, func(it NeedsYouItem) bool { return it.ID == in.Item })
	if i < 0 {
		return ActOut{}, fmt.Errorf("needs-you item %q is gone: its prompt was answered or changed; call needs_you again", in.Item)
	}
	it := items[i]
	if it.Kind != KindPrompt {
		return ActOut{}, fmt.Errorf("item %q is a %s, not a prompt; there is nothing to answer", in.Item, it.Kind)
	}
	if len(it.Options) == 0 {
		return ActOut{}, fmt.Errorf("item %q's options couldn't be read; peek %s", in.Item, it.Task)
	}
	if !slices.ContainsFunc(it.Options, func(o Option) bool { return o.N == in.Option }) {
		return ActOut{}, fmt.Errorf("option %d isn't one of item %q's options (1-%d)", in.Option, in.Item, len(it.Options))
	}
	if _, err := env.act.Call(ctx, "send_keys", map[string]any{"task": it.Task, "keys": []string{strconv.Itoa(in.Option)}}); err != nil {
		return ActOut{}, err
	}
	return ActOut{Message: fmt.Sprintf("picked option %d at %s's prompt; needs_you shows whether it is gone", in.Option, it.Task)}, nil
}

// sendKeys is safe-send: never into a pane where the owner has a draft,
// and never at a prompt, where answer binds the keys to the item.
func (env actEnv) sendKeys(ctx context.Context, in SendKeysIn) (ActOut, error) {
	if err := needTask(in.Task); err != nil {
		return ActOut{}, err
	}
	if in.Text == "" && len(in.Keys) == 0 {
		return ActOut{}, errors.New("give text or keys")
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	screen, err := env.act.Screen(in.Task)
	if err != nil {
		return ActOut{}, err
	}
	if app.DetectPrompt(stripControl(screen)) != app.PromptNone {
		return ActOut{}, fmt.Errorf("%s is at a prompt: answer it with answer {item, option} from needs_you", in.Task)
	}
	if tmux.HasDraft(screen) {
		return ActOut{}, fmt.Errorf("the owner has a draft in %s's input; not typing over it", in.Task)
	}
	args := map[string]any{"task": in.Task}
	if in.Text != "" {
		args["text"] = in.Text
	} else {
		args["keys"] = in.Keys
	}
	msg, err := env.act.Call(ctx, "send_keys", args)
	return ActOut{Message: msg}, err
}
