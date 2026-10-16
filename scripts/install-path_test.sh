#!/usr/bin/env bash
# Tests for scripts/install-path.sh (#666). Every case runs against a temp
# HOME and XDG_CONFIG_HOME, never the real ones.
set -uo pipefail
script="$(cd "$(dirname "$0")" && pwd)/install-path.sh"
d="$(mktemp -d)"; trap 'rm -rf "$d"' EXIT
fails=0
ok()   { echo "ok   $1"; }
fail() { echo "FAIL $1: $2"; fails=$((fails+1)); }

bin="$d/gobin"
sys_path="/usr/local/bin:/usr/bin:/bin"

# fresh makes a new empty HOME and sets $home.
n=0
fresh() { n=$((n+1)); home="$d/home$n"; mkdir -p "$home"; }

# run SHELLNAME PATH [VAR=VAL...] runs the script in $home with a clean env.
run() {
  local sh="$1" path="$2"; shift 2
  env -i HOME="$home" XDG_CONFIG_HOME="$home/.config" SHELL="/bin/$sh" PATH="$path" "$@" \
    bash "$script" "$bin" 2>&1
}

rcfile() {
  case "$1" in
    bash) echo "$home/.bashrc" ;;
    zsh) echo "$home/.zshrc" ;;
    fish) echo "$home/.config/fish/config.fish" ;;
  esac
}

for sh in bash zsh fish; do
  case "$sh" in
    fish) want="fish_add_path $bin" ;;
    *) want="export PATH=\"\$PATH:$bin\"" ;;
  esac

  # Already on PATH: silent, nothing written.
  fresh; rc="$(rcfile "$sh")"
  out="$(run "$sh" "$sys_path:$bin")"
  [[ -z "$out" && ! -e "$rc" ]] && ok "$sh: already on PATH is silent" || fail "$sh: on PATH" "$out"
  out="$(run "$sh" "$sys_path:$bin/")"
  [[ -z "$out" && ! -e "$rc" ]] && ok "$sh: trailing slash counts as on PATH" || fail "$sh: slash" "$out"

  # First install with an existing rc: backs it up, appends, keeps old lines.
  fresh; rc="$(rcfile "$sh")"; mkdir -p "$(dirname "$rc")"
  printf 'alias ll="ls -l"\nexport EDITOR=vi' >"$rc" # no trailing newline
  orig="$(cat "$rc")"
  out="$(run "$sh" "$sys_path")"
  grep -qxF "$want" "$rc" && ok "$sh: first install adds the line" || fail "$sh: first" "$(cat "$rc")"
  [[ "$(head -n2 "$rc")" == "$orig" ]] && ok "$sh: existing lines untouched" || fail "$sh: untouched" "$(cat "$rc")"
  [[ "$(cat "$rc.saddle-backup" 2>/dev/null)" == "$orig" ]] && ok "$sh: backs up before the first edit" || fail "$sh: backup" "$(ls -a "$(dirname "$rc")")"
  grep -q "Added $bin to PATH in $rc" <<<"$out" && grep -q "source $rc" <<<"$out" &&
    ok "$sh: says what changed and to source" || fail "$sh: message" "$out"

  # Second install: no duplicate, backup unchanged.
  before="$(cat "$rc")"
  out="$(run "$sh" "$sys_path")"
  [[ "$(cat "$rc")" == "$before" ]] && ok "$sh: second install adds nothing" || fail "$sh: second" "$(cat "$rc")"
  [[ "$(grep -cxF "$want" "$rc")" == 1 ]] && ok "$sh: exactly one line" || fail "$sh: count" "$(cat "$rc")"
  [[ "$(cat "$rc.saddle-backup")" == "$orig" ]] && ok "$sh: backup kept from the first edit" || fail "$sh: backup2" "$(cat "$rc.saddle-backup")"
  grep -q "already puts $bin on PATH" <<<"$out" && ok "$sh: second install says so" || fail "$sh: second msg" "$out"

  # First install with no rc yet: creates it, no backup.
  fresh; rc="$(rcfile "$sh")"
  run "$sh" "$sys_path" >/dev/null
  grep -qxF "$want" "$rc" && [[ ! -e "$rc.saddle-backup" ]] && ok "$sh: creates a missing rc" || fail "$sh: create" "$(ls -aR "$home")"

  # A PATH line the owner wrote already counts.
  fresh; rc="$(rcfile "$sh")"; mkdir -p "$(dirname "$rc")"
  case "$sh" in
    fish) echo "set -gx PATH $bin \$PATH" >"$rc" ;;
    *) echo "export PATH=$bin:\$PATH" >"$rc" ;;
  esac
  before="$(cat "$rc")"
  run "$sh" "$sys_path" >/dev/null
  [[ "$(cat "$rc")" == "$before" && ! -e "$rc.saddle-backup" ]] && ok "$sh: respects the owner's own PATH line" || fail "$sh: own line" "$(cat "$rc")"

  # Opt-out: nothing written, the line is printed.
  fresh; rc="$(rcfile "$sh")"
  out="$(run "$sh" "$sys_path" NO_PATH=1)"
  [[ ! -e "$rc" ]] && grep -qF "$want" <<<"$out" && ok "$sh: NO_PATH=1 only prints the line" || fail "$sh: opt-out" "$out"
done

# Unwritable rc: left alone, line printed. Root can write anything; skip there.
if [[ "$(id -u)" != 0 ]]; then
  fresh; rc="$home/.bashrc"; echo 'export A=1' >"$rc"; chmod 444 "$rc"
  out="$(run bash "$sys_path")"
  [[ "$(cat "$rc")" == 'export A=1' && ! -e "$rc.saddle-backup" ]] && grep -q "not writable" <<<"$out" &&
    ok "unwritable rc is left alone" || fail "unwritable" "$out"
  chmod 644 "$rc"
fi

# Unknown shell: nothing written, line printed.
fresh
out="$(run tcsh "$sys_path")"
[[ -z "$(ls -A "$home")" ]] && grep -qF "export PATH=\"\$PATH:$bin\"" <<<"$out" &&
  ok "unknown shell only prints the line" || fail "unknown shell" "$out"

# zsh honors ZDOTDIR.
fresh; mkdir -p "$home/zdot"
run zsh "$sys_path" ZDOTDIR="$home/zdot" >/dev/null
[[ -f "$home/zdot/.zshrc" && ! -e "$home/.zshrc" ]] && ok "zsh writes under ZDOTDIR" || fail "zdotdir" "$(ls -aR "$home")"

[ "$fails" -eq 0 ] || { echo "$fails failed"; exit 1; }
