#!/usr/bin/env bash
# Copies the upstream skills of the standard bundles into the FAM source tree.
# `make build` runs it. The copy is committed, so `go build` also works offline.
set -euo pipefail
cd "$(dirname "$0")/.."

# agent-browser: https://github.com/vercel-labs/agent-browser/tree/<ref>/skills/agent-browser
AGENT_BROWSER_REF="${AGENT_BROWSER_REF:-4720bc3dd73f2c8f7c84f415adfa560330b924c4}"
dest=internal/schema/fam/bundles/agent-browser/skill
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "https://codeload.github.com/vercel-labs/agent-browser/tar.gz/$AGENT_BROWSER_REF" |
  tar -xz -C "$tmp" --strip-components=3 "agent-browser-$AGENT_BROWSER_REF/skills/agent-browser"
test -f "$tmp/SKILL.md"
rm -rf "$dest"
mkdir -p "$dest"
cp -R "$tmp"/. "$dest"/
echo "agent-browser skill: $AGENT_BROWSER_REF" > "$dest/../UPSTREAM"
