#!/usr/bin/env bash
# The runtime against a real AIShiteru Core: the one pinned in
# .github/core-image (or CORE_BIN, a binary of Core), on a scratch database.
# The Go tests in e2e/ seat agents through Core's REST API as root, run the
# runtime with a scripted model, and watch the answers arrive in Core.
#
#   make e2e                                  (builds first)
#   CORE_BIN=../AIShiteru-Core/bin/aishiterud DATABASE_URL=postgres:///aishie_e2e scripts/e2e.sh
#
# When E2E_CORE_URL and E2E_ROOT_TOKEN are already set, that Core is used
# and nothing is started: it must be a throwaway.
set -euo pipefail
cd "$(dirname "$0")/.."

started=""
if [ -z "${E2E_CORE_URL:-}" ]; then
  export DATABASE_URL=${DATABASE_URL:-postgres:///aishie_e2e_core}
  export CORE_PORT=${CORE_PORT:-18090}
  dir=${CORE_DIR:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/aishiteru-ci-core}
  export CORE_DIR=${dir//\/\//\/}
  scripts/ci-core.sh start
  started=1
  trap 'scripts/ci-core.sh stop' EXIT
  # shellcheck disable=SC1091
  . "$CORE_DIR/env"
fi

echo "Core's catalogue is the snapshot the runtime was built against:"
bin/aishie-runtime catalogue --core "$E2E_CORE_URL" --check internal/core/testdata/catalogue.json

E2E_CORE_URL=$E2E_CORE_URL E2E_ROOT_TOKEN=$E2E_ROOT_TOKEN go test -count=1 -race -v ./e2e/ 2>&1 | tee "${CORE_DIR:-/tmp}/e2e.log"
[ -z "$started" ] || echo "Core's log is $CORE_DIR/core.log"
