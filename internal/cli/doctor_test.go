package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/doctor"
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
