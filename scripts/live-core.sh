#!/usr/bin/env bash
# The live tests that need a real AIshie Core and no model: Core's client
# over both transports, the agent runtime's hosting by an agent's id and its
# renditions (internal/core TestLiveContract), a real 429 waited out
# (TestLiveRateLimited), and a seat's toolset (internal/toolset
# TestLiveCore). They run against the Core pinned in .github/core-image (or
# CORE_BIN, a binary of Core), on a scratch database, started with a limit
# of CORE_RATE_LIMIT_PER_MINUTE calls a minute (600 by default), which the
# rate-limit test spends. CI does not run them: run them when the Core pin
# moves (CONTRIBUTING.md, Moving the Core pin) and before a release.
#
#   make live-core
#   CORE_BIN=../AIShie-Core/bin/aishie-core DATABASE_URL=postgres:///aishie_live_core scripts/live-core.sh
#
# When E2E_CORE_URL and E2E_ROOT_TOKEN are already set, that Core is used
# and nothing is started: it must be a throwaway (the tests make the agent
# runtime's credentials and claim its renditions), and
# CORE_RATE_LIMIT_PER_MINUTE names the limit it was started with, if any:
# without one, TestLiveRateLimited is skipped.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -z "${E2E_CORE_URL:-}" ]; then
  export DATABASE_URL=${DATABASE_URL:-postgres:///aishie_live_core}
  export CORE_PORT=${CORE_PORT:-18091}
  export CORE_RATE_LIMIT_PER_MINUTE=${CORE_RATE_LIMIT_PER_MINUTE:-600}
  # Apart from make e2e's, so that both may run at once.
  dir=${CORE_DIR:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/aishie-live-core}
  export CORE_DIR=${dir//\/\//\/}
  scripts/ci-core.sh start
  trap 'scripts/ci-core.sh stop' EXIT
  # shellcheck disable=SC1091
  . "$CORE_DIR/env"
fi

E2E_CORE_URL=$E2E_CORE_URL E2E_ROOT_TOKEN=$E2E_ROOT_TOKEN go test -count=1 -race -v -run '^TestLive' ./internal/core/ ./internal/toolset/
