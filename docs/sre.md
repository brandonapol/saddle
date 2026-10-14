# SRE config: `.saddle/sre.toml`

SRE mode keeps its per-repo knowledge in `.saddle/sre.toml`: the known
frequent problems to check preemptively, where to look, and which credentials
to use. The checks seed the runbook. Validate the file with
`saddle sre check-config`.

The file holds references only. A plaintext secret fails validation: token
shapes, passwords in URLs, secret-named keys and `--password=...` arguments
are all rejected. Use an env var name or a context/secret-manager ref.

```toml
[credentials.prod_db]
env = "PROD_DATABASE_URL"          # an environment variable name

[credentials.prod_cluster]
ref = "kube-context:prod-east"     # scheme:value (kube-context, vault, ...)

[endpoints.api]
url = "https://api.example.com/healthz"
credential = "prod_cluster"        # optional

[[check]]
name = "db-population"             # unique, lowercase [a-z0-9_-]
description = "users table is populated"
command = ["psql", "-At", "-c", "select count(*) from users"]
expect = "count greater than 0"
credentials = ["prod_db"]          # names from [credentials.*]

[[check]]
name = "api-health"
surface = "endpoint:api"           # alternative to command
expect = "HTTP 200"
```

- A check sets exactly one of `command` (argv, read-only) or `surface`, plus
  `expect`.
- A credential sets exactly one of `env` or `ref`.
- Unknown keys are errors, so typos do not pass silently.
- A missing file is an error from `saddle sre check-config`.

## Read-only production guard

While an incident is open (`.saddle/sre-incident.json`), its investigators are
read-only on production. saddle's PreToolUse hook classifies every tool call
of an investigator:

- **Read-only** calls run: file reads and searches, shell lines whose every
  command is a known reader (`kubectl get/describe/logs`, `curl` GET/HEAD,
  `aws ... describe-*/list-*/get-*`, `helm list/status`, `docker ps/logs`,
  `git log/diff/show`, `grep`, `cat`, ...), and the `command` of any
  `[[check]]` in `.saddle/sre.toml`, word for word. Edits and local `git
  add/commit` stay under the usual claims.
- **Everything else is mutating**: unknown programs and verbs, shell wrappers,
  `xargs`, substitutions, redirections that write a file, and MCP tools other
  than saddle's.

A mutating call is denied until one agent states a root cause and a
**different** agent verifies it:

```
saddle sre root-cause "migration 42 dropped users_email_idx" --evidence "seq scans in the slow log since 02:10"
saddle sre verify 1 --evidence "EXPLAIN on the replica shows the missing index"   # another investigator
saddle sre refute 1 --evidence "..."                                              # locks it again
saddle sre incident                                                               # the state
```

Once unlocked, a mutating call is still not run: it is filed in the decision
queue (`saddle decisions`, class `sre-action`, irreversible) naming the root
cause and both agents, with "Hold" recommended. If the owner picks "Run it",
the proposing investigator may run that exact command. A `[[preauth]]` rule
for `sre-action` takes the recommended option, which holds. `saddle sre
close` (owner or orchestrator) ends the incident and lifts the guard.
