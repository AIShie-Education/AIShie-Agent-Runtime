#!/usr/bin/env bash
# image_test.sh [IMAGE]: prove the single Go + Node 22 image as deployed:
# non-root entrypoint, configuration check, managed runner health, graceful
# shutdown, and absence of the removed Media toolchain.
set -euo pipefail

img=${1:-aishie-runtime:dev}
here=$(cd "$(dirname "$0")/.." && pwd)
cid=""
trap 'if [ -n "$cid" ]; then docker rm -f "$cid" >/dev/null 2>&1 || true; fi' EXIT
fail() { echo "image_test: $*" >&2; exit 1; }

got=$(docker image inspect --format '{{.Config.User}} {{json .Config.Entrypoint}} {{json .Config.Cmd}} {{json .Config.Env}}' "$img")
[[ "$got" == 65532:65532*'["/usr/local/bin/aishie-runtime"] ["run"]'* ]] ||
  fail "$img runs as, with and by: $got"
[[ "$got" == *'RUNNER_ENTRY=/opt/aishie/runner/dist/index.js'* ]] || fail "RUNNER_ENTRY is not set: $got"
docker run --rm "$img" version
docker run --rm --entrypoint node "$img" --version

# No Media executables or development packages are needed in production.
docker run --rm --entrypoint /bin/sh "$img" -ec \
  '! command -v soffice && ! command -v tesseract && ! command -v pdftoppm && ! command -v pdftocairo'

# check does not contact Core or a provider, and must work without Media.
out=$(docker run --rm --network none \
  -v "$here/examples":/config:ro -e CONFIG=/config/runtime.yaml,/config/agents "$img" check 2>&1) ||
  fail "check did not pass: $out"
grep -q '^runner: http://127.0.0.1:3001$' <<<"$out" || fail "check does not name the runner: $out"
grep -q '^the configuration passes: ' <<<"$out" || fail "check did not pass validation: $out"

# The actual run path starts Node on loopback, waits for health, and supervises
# it. Query health from inside the network-isolated container itself.
cid=$(docker run -d --network none \
  -v "$here/examples":/config:ro -e CONFIG=/config/runtime.yaml,/config/agents "$img" run)
healthy=""
for _ in $(seq 1 60); do
  if docker exec "$cid" node -e 'fetch("http://127.0.0.1:3001/healthz").then(async r=>{if(!r.ok)process.exit(1);const j=await r.json();if(j.status!=="ok")process.exit(1)}).catch(()=>process.exit(1))' >/dev/null 2>&1; then
    healthy=1
    break
  fi
  if [ "$(docker inspect -f '{{.State.Running}}' "$cid")" != true ]; then
    docker logs "$cid" >&2 || true
    fail "the runtime exited before runner health"
  fi
  sleep 0.25
done
[ -n "$healthy" ] || { docker logs "$cid" >&2 || true; fail "the managed runner did not become healthy"; }
docker stop -t 15 "$cid" >/dev/null || { docker logs "$cid" >&2 || true; fail "graceful stop failed"; }
wait_code=$(docker inspect -f '{{.State.ExitCode}}' "$cid")
[ "$wait_code" = 0 ] || { docker logs "$cid" >&2 || true; fail "run exited $wait_code"; }
docker rm "$cid" >/dev/null
cid=""
echo "image_test: $img passes"
