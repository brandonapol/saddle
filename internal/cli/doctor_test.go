package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/doctor"
	"github.com/brandonapol/saddle/internal/runq"
)

func TestDoctorExitCodes(t *testing.T) {
	okRow := doctor.Result{Name: "tmux", Status: doctor.OK, Detail: "tmux 3.7c"}
	warnRow := doctor.Result{Name: "leftovers", Status: doctor.Warn, Detail: "2 stale", Fix: "saddle gc"}
	failRow := doctor.Result{Name: "default branch", Status: doctor.Fail, Detail: "trunk", Fix: `set base = "trunk"`}
	for _, tc := range []struct {
		name    string
		rs      []doctor.Result
		asJSON  bool
		wantErr bool
	}{
		{"all ok", []doctor.Result{okRow}, false, false},
		{"warnings exit zero", []doctor.Result{okRow, warnRow}, false, false},
		{"a fail exits non-zero", []doctor.Result{okRow, warnRow, failRow}, false, true},
		{"json warn", []doctor.Result{warnRow}, true, false},
		{"json fail", []doctor.Result{failRow}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := reportDoctor(&out, tc.rs, tc.asJSON)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err %v, want error %v", err, tc.wantErr)
			}
			if tc.asJSON {
				var got struct {
					OK     bool            `json:"ok"`
					Checks []doctor.Result `json:"checks"`
				}
				if err := json.Unmarshal(out.Bytes(), &got); err != nil {
					t.Fatalf("not JSON: %v\n%s", err, out.String())
				}
				if got.OK == tc.wantErr || len(got.Checks) != len(tc.rs) {
					t.Fatalf("got %+v", got)
				}
				return
			}
			if !strings.Contains(out.String(), "STATUS") {
				t.Fatalf("no table:\n%s", out.String())
			}
		})
	}
}

func TestDoctorRegistered(t *testing.T) {
	cmd, _, err := Root().Find([]string{"doctor"})
	if err != nil || cmd.Name() != "doctor" || cmd.Flags().Lookup("json") == nil {
		t.Fatalf("doctor not registered with --json: %v", err)
	}
}

// TestShimCheck (#240): doctor passes healthy shims and warns when a pane's
// PATH puts the real tool before its shim.
func TestShimCheck(t *testing.T) {
	d := t.TempDir()
	shims, tools := filepath.Join(d, "shims"), filepath.Join(d, "tools")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	if r := shimCheck(shims, env(nil)); r.Status != doctor.OK {
		t.Fatalf("no shims yet: %+v", r)
	}
	if _, err := runq.WriteShims(shims, "/opt/saddle", runq.NewMatcher(runq.Config{}), tools); err != nil {
		t.Fatal(err)
	}
	if r := shimCheck(shims, env(map[string]string{"PATH": tools})); r.Status != doctor.OK {
		t.Fatalf("outside a pane: %+v", r)
	}
	r := shimCheck(shims, env(map[string]string{"PATH": tools + ":" + shims, "SADDLE_TASK": "t1"}))
	if r.Status != doctor.Warn || !strings.Contains(r.Detail, "before the go shim") {
		t.Fatalf("shadowed shim in a pane: %+v", r)
	}
}
