#!/usr/bin/env bash
# Real AR chat E2E: Core is used for identity/grants and MCP course tools;
# conversations, messages, attachments and generations stay in AR PostgreSQL.
# The test starts aishie-runtime and the Node 22 runner, with a local mock AI.
set -euo pipefail
cd "$(dirname "$0")/.."

make build
npm ci --prefix runner --prefer-offline --no-audit --no-fund
npm run build --prefix runner

started=""
if [ -z "${E2E_CORE_URL:-}" ]; then
  export DATABASE_URL=${DATABASE_URL:-postgres:///aishie_e2e_core}
  export CORE_PORT=${CORE_PORT:-18090}
  dir=${CORE_DIR:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/aishie-ci-core}
  export CORE_DIR=${dir//\/\//\/}
  scripts/ci-core.sh start
  started=1
  trap 'scripts/ci-core.sh stop' EXIT
  # shellcheck disable=SC1091
  . "$CORE_DIR/env"
fi

export E2E_AR_BIN=${E2E_AR_BIN:-$PWD/bin/aishie-runtime}
export E2E_RUNNER_ENTRY=${E2E_RUNNER_ENTRY:-$PWD/runner/dist/index.js}
export TEST_DATABASE_URL=${TEST_DATABASE_URL:-postgres:///postgres}
E2E_CORE_URL=$E2E_CORE_URL E2E_ROOT_TOKEN=$E2E_ROOT_TOKEN \
  E2E_RUNTIME_AUDIENCE=$E2E_RUNTIME_AUDIENCE E2E_AR_BIN=$E2E_AR_BIN E2E_RUNNER_ENTRY=$E2E_RUNNER_ENTRY \
  TEST_DATABASE_URL=$TEST_DATABASE_URL go test -count=1 -race -v ./e2e/ 2>&1 | tee "${CORE_DIR:-/tmp}/e2e.log"
[ -z "$started" ] || echo "Core's log is $CORE_DIR/core.log"
