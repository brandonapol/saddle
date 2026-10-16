#!/usr/bin/env bash
# install-path.sh DIR puts DIR (make install's $GOBIN) on PATH for the login
# shell, once (#666). It says nothing when DIR is already on PATH. Otherwise
# it appends one marked line to the shell's config:
#   bash  ~/.bashrc                        export PATH="$PATH:DIR"
#   zsh   ${ZDOTDIR:-~}/.zshrc             export PATH="$PATH:DIR"
#   fish  ${XDG_CONFIG_HOME:-~/.config}/fish/config.fish   fish_add_path DIR
# It only ever appends: existing lines are never rewritten. The file is copied
# to FILE.saddle-backup before the first edit. A file that already has the
# marker, or a PATH line naming DIR, is left alone. An unwritable file, an
# unknown shell or NO_PATH=1 (make install NO_PATH=1) only prints the line to
# add by hand. It never fails the install.
set -euo pipefail

dir="${1:?usage: install-path.sh DIR}"
dir="${dir%/}"
marker="# Added by saddle (make install): put saddle on PATH"

case ":${PATH:-}:" in
  *":$dir:"* | *":$dir/:"*) exit 0 ;;
esac

shell="$(basename "${SHELL:-}")"
case "$shell" in
  bash) rc="$HOME/.bashrc" line="export PATH=\"\$PATH:$dir\"" ;;
  zsh) rc="${ZDOTDIR:-$HOME}/.zshrc" line="export PATH=\"\$PATH:$dir\"" ;;
  fish) rc="${XDG_CONFIG_HOME:-$HOME/.config}/fish/config.fish" line="fish_add_path $dir" ;;
  *) rc="" line="export PATH=\"\$PATH:$dir\"" ;;
esac

# by_hand WHY prints the line to add and stops.
by_hand() {
  echo "Note: $dir is not on your PATH, so 'saddle' won't run by name. $1"
  if [[ -n "$rc" ]]; then
    echo "  Add this line to $rc, then open a new shell:"
  else
    echo "  Add this line to your shell's config, then open a new shell:"
  fi
  echo "    $line"
  exit 0
}

if [[ -n "${NO_PATH:-}" && "${NO_PATH}" != 0 ]]; then
  by_hand "(NO_PATH is set, so your shell config was left alone.)"
fi
if [[ -z "$rc" ]]; then
  by_hand "(Unknown shell '${shell:-unset}', so no config file was edited.)"
fi

if [[ -f "$rc" ]] && { grep -qF "$marker" "$rc" || grep -F "$dir" "$rc" | grep -qE 'PATH|fish_add_path'; }; then
  echo "Note: $rc already puts $dir on PATH. Open a new shell or run: source $rc"
  exit 0
fi

if [[ -e "$rc" && ! -w "$rc" ]]; then
  by_hand "($rc is not writable, so it was left alone.)"
fi
if [[ ! -e "$rc" ]] && ! mkdir -p "$(dirname "$rc")" 2>/dev/null; then
  by_hand "(Could not create $(dirname "$rc"), so nothing was written.)"
fi
if [[ ! -e "$rc" && ! -w "$(dirname "$rc")" ]]; then
  by_hand "($(dirname "$rc") is not writable, so nothing was written.)"
fi

backup=""
if [[ -s "$rc" ]]; then
  backup="$rc.saddle-backup"
  if [[ ! -e "$backup" ]]; then
    cp -p "$rc" "$backup"
  fi
fi

{
  # Start on a fresh line if the file doesn't end with one.
  if [[ -s "$rc" && -n "$(tail -c1 "$rc")" ]]; then echo; fi
  echo
  echo "$marker"
  echo "$line"
} >>"$rc"

echo "Added $dir to PATH in $rc${backup:+ (backup: $backup)}:"
echo "    $line"
echo "  Open a new shell or run: source $rc"
