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
