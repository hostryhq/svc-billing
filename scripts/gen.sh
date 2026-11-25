#!/usr/bin/env bash
# Regenerates internal/gen from the api-specs checkout (default ../api-specs; override API_SPECS_DIR).
# Uses the oapi-codegen version pinned by api-specs (its Makefile) and records the spec commit.
set -euo pipefail
cd "$(dirname "$0")/.."
SPECS="${API_SPECS_DIR:-../api-specs}"
OAPI="${OAPI_CODEGEN:-$SPECS/.tools/bin/oapi-codegen}"
REDOCLY_VERSION=2.83.0
mkdir -p .tmp internal/gen
npx --yes "@redocly/cli@${REDOCLY_VERSION}" bundle "$SPECS/openapi/billing/v1/openapi.yaml" -o .tmp/billing.yaml >/dev/null
"$OAPI" -config oapi-codegen.yaml .tmp/billing.yaml
go run ./tools/seedgen -in .tmp/billing.yaml -out internal/seed/examples.json
cp "$SPECS/tax/conformance/vectors.v1.json" tax/vectors/vectors.v1.json
cp "$SPECS/tax/rule-sets/draft-unreviewed.v1.yaml" tax/vectors/draft-unreviewed.v1.yaml
go run ./tools/gen -spec "$SPECS/openapi/billing/v1/openapi.yaml" -permissions "$SPECS/common/permissions.yaml" -out internal/gen
{
  echo "api-specs commit: $(git -C "$SPECS" rev-parse HEAD)"
  echo "oapi-codegen: $("$OAPI" --version | tail -1)"
} > internal/gen/SPEC_VERSION
echo "generated from $(head -1 internal/gen/SPEC_VERSION)"
