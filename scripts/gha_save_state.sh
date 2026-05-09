#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_BRANCH="${STATE_BRANCH:-automation-state}"
WORKTREE_DIR="$(mktemp -d)"

cleanup() {
  git -C "$ROOT_DIR" worktree remove --force "$WORKTREE_DIR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "[state] save branch: ${STATE_BRANCH}"

if git -C "$ROOT_DIR" ls-remote --exit-code --heads origin "$STATE_BRANCH" >/dev/null 2>&1; then
  git -C "$ROOT_DIR" fetch --depth=1 origin "$STATE_BRANCH"
  git -C "$ROOT_DIR" worktree add --detach "$WORKTREE_DIR" "origin/${STATE_BRANCH}" >/dev/null
else
  git -C "$ROOT_DIR" worktree add --detach "$WORKTREE_DIR" HEAD >/dev/null
  git -C "$WORKTREE_DIR" checkout --orphan "$STATE_BRANCH" >/dev/null 2>&1
fi

find "$WORKTREE_DIR" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +

copy_path() {
  local rel="$1"
  local src="$ROOT_DIR/$rel"
  local dst="$WORKTREE_DIR/$rel"

  if [ ! -e "$src" ]; then
    return 0
  fi

  mkdir -p "$(dirname "$dst")"
  cp -R "$src" "$dst"
  echo "[state] staged $rel"
}

copy_glob() {
  local pattern="$1"
  shopt -s nullglob
  local matches=("$ROOT_DIR"/$pattern)
  shopt -u nullglob

  local match
  for match in "${matches[@]}"; do
    local rel="${match#"$ROOT_DIR"/}"
    local dst="$WORKTREE_DIR/$rel"
    mkdir -p "$(dirname "$dst")"
    cp -R "$match" "$dst"
    echo "[state] staged $rel"
  done
}

copy_path "data/briefing.db"
copy_path "data/sent_urls.txt"
copy_path "data/sent_titles.txt"
copy_path "data/images/cards"
copy_path "daily"
copy_path "docs"
copy_glob "data/slack-payload-*.json"
copy_glob "data/slack-weekly-payload-*.json"
copy_glob "data/feishu-daily-card-*.json"
copy_glob "data/feishu-weekly-card-*.json"

cat > "$WORKTREE_DIR/README.md" <<'EOF'
# automation-state

This branch is maintained by GitHub Actions.
It stores generated state needed for fully automated daily/weekly runs:

- `data/briefing.db`
- `data/sent_urls.txt`
- `data/sent_titles.txt`
- `data/slack-payload-*.json`
- `data/slack-weekly-payload-*.json`
- `data/feishu-daily-card-*.json`
- `data/feishu-weekly-card-*.json`
- `data/images/cards/`
- `daily/`
- `docs/`
EOF

git -C "$WORKTREE_DIR" add -A
if git -C "$WORKTREE_DIR" diff --cached --quiet; then
  echo "[state] no changes to commit"
  exit 0
fi

git -C "$WORKTREE_DIR" commit -m "chore(state): update automation snapshot" >/dev/null
git -C "$WORKTREE_DIR" push origin "HEAD:${STATE_BRANCH}"
echo "[state] pushed ${STATE_BRANCH}"
