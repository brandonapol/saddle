#!/usr/bin/env bash
# Prompt for the credentials saddle needs, but only the ones that are missing:
#   JEV_TOKEN        TypeSafe Jev key for attention triage (optional)
#   Claude Code auth an existing `claude` login, or CLAUDE_CODE_OAUTH_TOKEN
# Tokens are read without echo and appended to your shell rc file. Nothing is
# printed back. Without a terminal (CI), it only reports what it found.
set -euo pipefail

case "$(basename "${SHELL:-bash}")" in
  zsh) default_rc="$HOME/.zshrc" ;;
  *) default_rc="$HOME/.bashrc" ;;
esac
RC="${SHELL_RC:-$default_rc}"
touch "$RC"

ok() { printf '  \033[32m✓\033[0m %s\n' "$1"; }
note() { printf '  \033[33m•\033[0m %s\n' "$1"; }

interactive=1
if [[ ! -t 0 || -n "${CI:-}" ]]; then
  interactive=0
fi

# in_rc VAR reports whether RC exports VAR (the value is never read out).
in_rc() { grep -qE "^[[:space:]]*export[[:space:]]+$1=" "$RC"; }

# save VAR VALUE appends a quoted export to RC.
save() {
  printf '\n# Added by saddle (make setup)\nexport %s=%q\n' "$1" "$2" >>"$RC"
  ok "saved $1 to $RC"
  wrote=1
}

# ask PROMPT reads a secret into $reply without echoing it.
ask() {
  reply=""
  read -r -s -p "  $1" reply || true
  echo
}

wrote=0
echo "saddle credentials (shell rc: $RC)"

# Jev
if [[ -n "${JEV_TOKEN:-}${TYPESAFE_API_KEY:-}" ]] || in_rc JEV_TOKEN || in_rc TYPESAFE_API_KEY; then
  ok "Jev key found: attention triage is on"
elif [[ $interactive == 1 ]]; then
  echo "  Jev (TypeSafe) triages agent events so you're only pinged when it matters."
  echo "  Get a key at https://console.typesafe.ai. Press enter to skip."
  ask "JEV_TOKEN: "
  if [[ -n "$reply" ]]; then
    save JEV_TOKEN "$reply"
  else
    note "skipped Jev: triage stays off (run make setup/tokens later)"
  fi
else
  note "no JEV_TOKEN: triage is off"
fi

# Claude Code
if ! command -v claude >/dev/null 2>&1; then
  note "claude is not installed. See https://docs.claude.com/en/docs/claude-code"
elif claude auth status --json 2>/dev/null | grep -q '"loggedIn": *true'; then
  ok "Claude Code is logged in"
elif [[ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}${ANTHROPIC_API_KEY:-}" ]] || in_rc CLAUDE_CODE_OAUTH_TOKEN || in_rc ANTHROPIC_API_KEY; then
  ok "Claude token found"
elif [[ $interactive == 1 ]]; then
  echo "  Claude Code isn't logged in. Saddle's agents run as you."
  echo "    1) log in now (claude auth login, opens a browser)"
  echo "    2) paste a long-lived token (create one with: claude setup-token)"
  echo "    3) skip"
  read -r -p "  choose [1]: " choice || true
  case "${choice:-1}" in
    1) claude auth login ;;
    2)
      ask "CLAUDE_CODE_OAUTH_TOKEN: "
      if [[ -n "$reply" ]]; then
        save CLAUDE_CODE_OAUTH_TOKEN "$reply"
      else
        note "no token entered"
      fi
      ;;
    *) note "skipped Claude login: saddle up won't start agents until you log in" ;;
  esac
else
  note "Claude Code is not logged in (run: claude auth login)"
fi

if [[ $wrote == 1 ]]; then
  echo "  Run 'source $RC' or open a new shell to pick up the new variables."
fi
