#!/usr/bin/env bash
# Generates the Go structs in internal/pklgen from the Pkl schema.
# Needs the pinned pkl binary. Run `go run ./cmd/fam version` once to download it.
set -euo pipefail
cd "$(dirname "$0")/.."
PKL="${PKL:-$(go run ./cmd/fam internal pkl-path)}"
G=github.com/gregor-tokarev/fam/internal/pklgen
rm -rf internal/pklgen
# One Go package for each module: each module gets its own Load function.
args=()
for m in Config base Bundle tasks Machines Agents; do
  args+=(--mapping "fam.$m=$G/$(echo "$m" | tr '[:upper:]' '[:lower:]')")
done
"$PKL" run package://pkg.pkl-lang.org/pkl-go/pkl.golang@0.14.0#/gen.pkl \
  internal/schema/fam/Config.pkl internal/schema/fam/Machines.pkl internal/schema/fam/Agents.pkl \
  "${args[@]}" --base-path github.com/gregor-tokarev/fam >/dev/null
gofmt -w internal/pklgen
