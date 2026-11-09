#!/usr/bin/env bash
# lint — static checks for Go and the console.
set -euo pipefail
cd "$(dirname "$0")/.."
GO=${GO:-go}

unformatted=$(gofmt -l $(git ls-files --cached --others --exclude-standard '*.go'))
if [ -n "$unformatted" ]; then
  echo "lint: gofmt needed:"; echo "$unformatted"; exit 1
fi
$GO vet ./...
if command -v staticcheck >/dev/null 2>&1; then
  staticcheck ./...
elif [ -x "$($GO env GOPATH)/bin/staticcheck" ]; then
  "$($GO env GOPATH)/bin/staticcheck" ./...
else
  echo "lint: staticcheck not installed (go install honnef.co/go/tools/cmd/staticcheck@v0.8.2)"; exit 1
fi
if [ -d web/node_modules ]; then
  # The console's API types are generated from api/openapi.yaml; a stale
  # copy would let the console call routes the spec (and server) lack.
  (cd web && cp src/lib/api/schema.gen.ts "${TMPDIR:-/tmp}/plugrelay-schema.gen.ts" \
    && npm run --silent gen:api >/dev/null \
    && if ! cmp -s src/lib/api/schema.gen.ts "${TMPDIR:-/tmp}/plugrelay-schema.gen.ts"; then
         echo "lint: web/src/lib/api/schema.gen.ts was stale; regenerated — review and keep it"; exit 1
       fi)
  (cd web && npm run --silent typecheck && npm run --silent lint)
else
  echo "lint: web/node_modules missing — run 'cd web && npm ci'"; exit 1
fi
echo "lint: green"
