package mcpserver

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

// #464: a worker files an owner decision through the decisions tool and it
// reaches the orchestrator; only the orchestrator answers or pre-authorizes,
// and a rule it adds closes the waiting card and tells the worker to go on.
func TestDecisionsTool(t *testing.T) {
	a, _ := stackSetup(t)
	_, _ = a.Store.TakeNotices(app.OrchestratorID, false)
	file := map[string]any{"action": "file", "class": "dependency", "subject": "github.com/aws/aws-sdk-go-v2",
		"question": "Add the AWS SDK?", "options": []map[string]any{{"label": "Add it", "action": "go get github.com/aws/aws-sdk-go-v2"}}}
	var out DecisionsOut
	if res := callTool(t, a, "t1", "decisions", file, &out); res.IsError {
		t.Fatalf("file: %s", errText(res))
	}
	if len(out.Decisions) != 1 || !out.Decisions[0].Open() || !strings.Contains(out.Message, "Keep every other lane running") {
		t.Fatalf("file = %+v", out)
	}
	id := out.Decisions[0].ID
	ns, _ := a.Store.TakeNotices(app.OrchestratorID, true)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "Add the AWS SDK?") || !strings.Contains(ns[0].Text, "Add it (recommended)") {
		t.Fatalf("orchestrator notices = %+v", ns)
	}

	if res := callTool(t, a, "t1", "decisions", map[string]any{"action": "allow", "class": "dependency"}, nil); !res.IsError {
		t.Fatal("a worker pre-authorized a class")
	}
	if res := callTool(t, a, "t1", "decisions", map[string]any{"action": "answer", "id": id, "option": 1}, nil); !res.IsError {
		t.Fatal("a worker answered a decision")
	}

	out = DecisionsOut{}
	if res := callTool(t, a, app.OrchestratorID, "decisions", map[string]any{"action": "allow", "class": "dependency", "match": "github.com/aws/*"}, &out); res.IsError {
		t.Fatalf("allow: %s", errText(res))
	}
	if len(out.Decisions) != 1 || out.Decisions[0].ID != id || out.Decisions[0].By != app.DecidedPreauth {
		t.Fatalf("allow = %+v", out)
	}
	ns, _ = a.Store.TakeNotices("t1", true)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "Go ahead: go get github.com/aws/aws-sdk-go-v2") {
		t.Fatalf("t1 notices = %+v", ns)
	}
}
