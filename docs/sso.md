# SSO logins for codex agents

Saddle can log in once to an OIDC identity provider (Keycloak, Okta, or any
provider with OIDC discovery and the device authorization grant) and hand
every codex agent a fresh bearer token for a model gateway behind that
provider. You sign in once when `saddle up` starts. Workers never log in
themselves (#758).

## How it works

Codex 0.154+ can get a model provider's bearer token from a command:
`[model_providers.<id>.auth] command/args`. It re-runs that command every
`refresh_interval_ms` and after a 401. When `[sso.<name>]` sets
`codex_provider`, saddle adds `-c` overrides to every codex launch,
interactive and headless, so the command is `saddle sso token --root <repo> <name>`:

    -c model_provider="corp-gw"
    -c model_providers.corp-gw.auth.command="/path/to/saddle"
    -c model_providers.corp-gw.auth.args=["sso","token","--root","/repo","corp"]
    -c model_providers.corp-gw.auth.timeout_ms=15000
    -c model_providers.corp-gw.auth.refresh_interval_ms=60000

`saddle sso token` prints a token with at least a minute left. It uses the
cached one or refreshes it with the refresh token. Concurrent callers take a
file lock, so a rotating refresh token gets used only once.

No token is written to `launch.sh`, `codex.json`, env vars, events, notices
or briefs. Tokens live in one file per issuer, client, scopes and audience
under `$XDG_CACHE_HOME/saddle/sso/` (or `$SADDLE_SSO_DIR`), with mode 0600
in a 0700 dir. Every repo using the same issuer and client shares one login.

## Setup

1. In the IdP, create a **public** client with the device authorization grant
   on. In Keycloak that's "OAuth 2.0 Device Authorization Grant" under the
   client's capability config. In Okta it's a Native app with the "Device
   Authorization" grant type. To get a refresh token, allow `offline_access`
   (Okta: turn on "Refresh Token").
2. Configure it in `.saddle/config.toml` or `~/.config/saddle/config.toml`:

   ```toml
   [sso.corp]
   issuer = "https://keycloak.example.com/realms/eng"   # Okta: https://<org>.okta.com/oauth2/default
   client_id = "saddle-cli"
   codex_provider = "corp-gw"
   base_url = "https://llm-gateway.example.com/v1"      # optional, see below
   ```

   Without `base_url`, define the provider in `~/.codex/config.toml` (its
   `base_url`, `wire_api`, …). It must not set `env_key`,
   `experimental_bearer_token` or `requires_openai_auth`, because codex refuses to
   combine those with a command-backed token.
3. Run `saddle sso login` (or just `saddle up`, which runs it when there is
   no usable login). Open the URL it prints and enter the code.

`saddle sso status` shows each login and its expiry. It never prints a
token. `saddle sso logout` forgets a login.

## When the login lapses

If the refresh token expires or is revoked mid-run, the next
`saddle sso token` call does three things:

- starts one device sign-in and saves it as pending (mode 0600),
- starts a detached `saddle sso login --pending` that waits for the approval,
- exits non-zero with the URL and code, which codex shows as the turn's error.

The background login sends the URL and code to the orchestrator as an
action notice, and to `[notify]` desktop/webhook when set. Later calls reuse
the pending sign-in and don't send repeat notices. Once you approve, the
tokens are cached and agents pick them up on their next request or turn.

While no login is usable, `saddle status` and spawns show codex as
unavailable with `run saddle sso login <name>`.

## Limits

- Only codex uses `[sso]` today. Claude, grok, gemini and opencode keep
  their own logins.
- At most one `[sso]` table may set `codex_provider`.
- `saddle up` signs in only when it runs on a terminal. Otherwise it warns,
  and agents ask through the orchestrator instead.
