package sre

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const valid = `
[credentials.prod_db]
env = "PROD_DATABASE_URL"

[credentials.prod_cluster]
ref = "kube-context:prod-east"

[endpoints.api]
url = "https://api.example.com/healthz"
credential = "prod_cluster"

[[check]]
name = "db-population"
description = "users table is populated"
command = ["psql", "-At", "-c", "select count(*) from users"]
expect = "count greater than 0"
credentials = ["prod_db"]

[[check]]
name = "api-health"
surface = "endpoint:api"
expect = "HTTP 200"
`

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Checks) != 2 || c.Checks[0].Name != "db-population" || len(c.Checks[0].Command) != 4 {
		t.Fatalf("checks = %+v", c.Checks)
	}
	if c.Credentials["prod_db"].Env != "PROD_DATABASE_URL" || c.Credentials["prod_cluster"].Ref != "kube-context:prod-east" {
		t.Fatalf("credentials = %+v", c.Credentials)
	}
	if c.Endpoints["api"].Credential != "prod_cluster" {
		t.Fatalf("endpoints = %+v", c.Endpoints)
	}
}

func TestParseInvalid(t *testing.T) {
	cases := map[string]struct{ toml, want string }{
		"bad toml":          {`[[check`, "sre.toml"},
		"unknown key":       {"[[check]]\nname=\"a\"\ncommand=[\"x\"]\nexpect=\"y\"\nbogus=1", "bogus"},
		"no name":           {"[[check]]\ncommand=[\"x\"]\nexpect=\"y\"", "name"},
		"bad name":          {"[[check]]\nname=\"Has Space\"\ncommand=[\"x\"]\nexpect=\"y\"", "name"},
		"duplicate name":    {"[[check]]\nname=\"a\"\ncommand=[\"x\"]\nexpect=\"y\"\n[[check]]\nname=\"a\"\ncommand=[\"x\"]\nexpect=\"y\"", "duplicate"},
		"no command":        {"[[check]]\nname=\"a\"\nexpect=\"y\"", "command or surface"},
		"both":              {"[[check]]\nname=\"a\"\ncommand=[\"x\"]\nsurface=\"s\"\nexpect=\"y\"", "not both"},
		"no expect":         {"[[check]]\nname=\"a\"\ncommand=[\"x\"]", "expect"},
		"empty argv":        {"[[check]]\nname=\"a\"\ncommand=[]\nexpect=\"y\"", "command or surface"},
		"unknown cred":      {"[[check]]\nname=\"a\"\ncommand=[\"x\"]\nexpect=\"y\"\ncredentials=[\"nope\"]", "nope"},
		"cred neither":      {"[credentials.c]\n", "env or ref"},
		"cred both":         {"[credentials.c]\nenv=\"A\"\nref=\"vault:x\"", "env or ref"},
		"bad env name":      {"[credentials.c]\nenv=\"not an env var\"", "env"},
		"bad ref":           {"[credentials.c]\nref=\"nocolon\"", "scheme:value"},
		"endpoint no url":   {"[endpoints.e]\n", "url"},
		"endpoint bad url":  {"[endpoints.e]\nurl=\"not a url\"", "url"},
		"endpoint bad cred": {"[endpoints.e]\nurl=\"https://x.io\"\ncredential=\"zzz\"", "zzz"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.toml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

// Secret-shaped fixtures are built at runtime so no commit holds a whole one.
func TestParseRejectsPlaintextSecrets(t *testing.T) {
	ghp := "gh" + "p_abcdefghijklmnopqrstuvwxyz0123456789"
	cases := map[string]string{
		"token literal in expect":  "[[check]]\nname=\"a\"\ncommand=[\"x\"]\nexpect=\"" + ghp + "\"",
		"token literal in command": "[[check]]\nname=\"a\"\ncommand=[\"curl\",\"-H\",\"" + ghp + "\"]\nexpect=\"y\"",
		"token in env field":       "[credentials.c]\nenv=\"" + ghp + "\"",
		"password in url":          "[endpoints.e]\nurl=\"postgres://admin:hunter2@db.example.com/app\"",
		"secret-named key":         "[credentials.c]\nenv=\"A\"\npassword=\"hunter2\"",
		"token-named key":          "[endpoints.e]\nurl=\"https://x.io\"\napi_key=\"abc\"",
		"inline password flag":     "[[check]]\nname=\"a\"\ncommand=[\"mysql\",\"--password=hunter2\"]\nexpect=\"y\"",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(src))
			if err == nil || !strings.Contains(err.Error(), "secret") {
				t.Fatalf("err = %v, want plaintext-secret rejection", err)
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), ghp) {
				t.Fatalf("error leaks the secret: %v", err)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	root := t.TempDir()
	if _, err := Load(root); !os.IsNotExist(unwrapAll(err)) {
		t.Fatalf("missing file: err = %v, want not-exist", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".saddle", "sre.toml"), []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(root)
	if err != nil || len(c.Checks) != 2 {
		t.Fatalf("Load = %+v, %v", c, err)
	}
}

func unwrapAll(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = u.Unwrap()
	}
}
