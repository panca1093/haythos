#!/usr/bin/env bash
# Installs gbrain at a pinned commit, creates a keyless local brain, and imports the MyBook wiki folders that are safe to share.
# Safe to re-run: every step checks first. Nothing here needs an API key; nothing leaves this machine.
set -euo pipefail

# Commit read when this script was written (garrytan/gbrain master, 2026-10-28). Bump it deliberately.
GBRAIN_COMMIT="613400c3fa9b0635da8c4cd0bae640543cb3e2c6"
GBRAIN_REPO="https://github.com/garrytan/gbrain.git"
GBRAIN_SRC="${GBRAIN_SRC:-$HOME/.haythos/gbrain}"
WIKI_DIR="${WIKI_DIR:-$HOME/Documents/MyBook/wiki}"
# Decision 5: only these folders and index.md. Never personal/, never loose top-level pages.
WIKI_FOLDERS=(entities concepts tickets learnings)

say() { printf '==> %s\n' "$*"; }

command -v bun >/dev/null || { echo "bun is required (https://bun.sh)"; exit 1; }
command -v git >/dev/null || { echo "git is required"; exit 1; }
[ -d "$WIKI_DIR" ] || { echo "wiki not found at $WIKI_DIR (set WIKI_DIR)"; exit 1; }

say "gbrain source: $GBRAIN_SRC at $GBRAIN_COMMIT"
if [ ! -d "$GBRAIN_SRC/.git" ]; then
  say "cloning $GBRAIN_REPO"
  git clone --quiet "$GBRAIN_REPO" "$GBRAIN_SRC"
fi
if [ "$(git -C "$GBRAIN_SRC" rev-parse HEAD)" != "$GBRAIN_COMMIT" ]; then
  say "checking out pinned commit"
  git -C "$GBRAIN_SRC" fetch --quiet origin "$GBRAIN_COMMIT" || git -C "$GBRAIN_SRC" fetch --quiet origin
  git -C "$GBRAIN_SRC" checkout --quiet "$GBRAIN_COMMIT"
  rm -rf "$GBRAIN_SRC/node_modules"
fi
if [ ! -d "$GBRAIN_SRC/node_modules" ] || ! command -v gbrain >/dev/null; then
  say "bun install + bun link (autopilot daemon install disabled)"
  (cd "$GBRAIN_SRC" && GBRAIN_NO_AUTOPILOT_INSTALL=1 bun install && bun link)
fi
command -v gbrain >/dev/null || { echo "gbrain is not on PATH; add \$HOME/.bun/bin to PATH and re-run"; exit 1; }
say "gbrain: $(gbrain --version 2>/dev/null || echo installed)"

# TODO: uncertain — brain existence is detected by the default state dir; verify `gbrain init` is not safe to re-run before relying on this
if [ -d "${GBRAIN_HOME:-$HOME}/.gbrain" ]; then
  say "brain already initialised, skipping init"
else
  say "gbrain init --pglite --no-embedding (keyless, local)"
  gbrain init --pglite --no-embedding
fi

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
total=0
for d in "${WIKI_FOLDERS[@]}"; do
  if [ -d "$WIKI_DIR/$d" ]; then
    cp -R "$WIKI_DIR/$d" "$stage/$d"
    n=$(find "$stage/$d" -name '*.md' | wc -l | tr -d ' ')
    say "staged $d/ ($n pages)"
    total=$((total + n))
  else
    say "skipping $d/ (not found)"
  fi
done
if [ -f "$WIKI_DIR/index.md" ]; then
  cp "$WIKI_DIR/index.md" "$stage/index.md"
  say "staged index.md"
  total=$((total + 1))
fi
[ "$total" -gt 0 ] || { echo "nothing to import"; exit 1; }

say "importing $total pages (--no-embed: keyword search only)"
gbrain import "$stage" --no-embed

say "done. Add the gbrain server to config/haythos.yaml (see README, Brain section) and restart haythosd."
