#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_BRANCH="${STATE_BRANCH:-automation-state}"
WORKTREE_DIR="$(mktemp -d)"

cleanup() {
  git -C "$ROOT_DIR" worktree remove --force "$WORKTREE_DIR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "[state] restore branch: ${STATE_BRANCH}"

if ! git -C "$ROOT_DIR" ls-remote --exit-code --heads origin "$STATE_BRANCH" >/dev/null 2>&1; then
  echo "[state] branch does not exist yet, skipping restore"
  exit 0
fi

git -C "$ROOT_DIR" fetch --depth=1 origin "$STATE_BRANCH"
git -C "$ROOT_DIR" worktree add --detach "$WORKTREE_DIR" "origin/${STATE_BRANCH}" >/dev/null

restore_path() {
  local rel="$1"
  local src="$WORKTREE_DIR/$rel"
  local dst="$ROOT_DIR/$rel"

  if [ ! -e "$src" ]; then
    return 0
  fi

  rm -rf "$dst"
  mkdir -p "$(dirname "$dst")"
  cp -R "$src" "$dst"
  echo "[state] restored $rel"
}

restore_glob() {
  local pattern="$1"
  shopt -s nullglob
  local matches=("$WORKTREE_DIR"/$pattern)
  shopt -u nullglob

  local match
  for match in "${matches[@]}"; do
    local rel="${match#"$WORKTREE_DIR"/}"
    local dst="$ROOT_DIR/$rel"
    rm -rf "$dst"
    mkdir -p "$(dirname "$dst")"
    cp -R "$match" "$dst"
    echo "[state] restored $rel"
  done
}

restore_path "data/briefing.db"
restore_path "data/sent_urls.txt"
restore_path "data/sent_titles.txt"
restore_path "data/images/cards"
restore_path "daily"
restore_path "docs"
restore_glob "data/slack-payload-*.json"
restore_glob "data/slack-weekly-payload-*.json"
restore_glob "data/feishu-daily-card-*.json"
restore_glob "data/feishu-weekly-card-*.json"
