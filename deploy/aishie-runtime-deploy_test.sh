#!/usr/bin/env bash
# aishie-runtime-deploy against stand-ins for docker, pg_dump (by way of
# runuser), curl, flock and sleep, which record what they are asked to do:
#
#   make script-test
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

REPO=ghcr.io/aishie-education/aishie-agent-runtime
IMG=$REPO:0.2.0
OLD=$REPO:0.1.0

# The stand-ins. Each appends its command line to $CALLS; docker keeps the
# image of the container named aishie-runtime in $STATE/container, and its
# stop timeout in $STATE/stop-timeout. An image's id is its name, unless
# $STATE/ids says otherwise ("name id" lines: one image under two names), and
# its version is $VERSION_OUT, unless $STATE/versions says otherwise ("name
# version line").
mkdir -p "$work/bin"
cat > "$work/bin/docker" <<'EOF'
#!/usr/bin/env bash
echo "docker $*" >> "$CALLS"
id_of() { awk -v n="$1" '$1 == n { print $2; f = 1 } END { if (!f) print "id:" n }' "$STATE/ids" 2>/dev/null || echo "id:$1"; }
version_of() {
  v=$(awk -v n="$1" '$1 == n { $1 = ""; sub(/^ /, ""); print }' "$STATE/versions" 2>/dev/null)
  echo "${v:-$VERSION_OUT}"
}
case $1 in
  pull) exit "${PULL_FAIL:-0}" ;;
  inspect)
    [ -e "$STATE/container" ] || exit 1
    case $3 in
      '{{.Image}}') id_of "$(cat "$STATE/container")" ;;
      '{{.Config.StopTimeout}}') cat "$STATE/stop-timeout" 2>/dev/null || echo '<nil>' ;;
      *) cat "$STATE/container" ;;
    esac ;;
  image) [ "$2" = inspect ] && id_of "${*: -1}" ;;
  stop) : ;;
  rm) rm -f "$STATE/container" "$STATE/stop-timeout" ;;
  run)
    last=${*: -1}
    if [ "$2" = -d ]; then
      [ ! -e "$STATE/container" ] || { echo "docker: the name aishie-runtime is taken" >&2; exit 125; }
      echo "$last" > "$STATE/container"
      while [ $# -gt 0 ]; do [ "$1" = --stop-timeout ] && { echo "$2" > "$STATE/stop-timeout"; break; }; shift; done
    elif [ "$last" = version ]; then
      version_of "${*: -2:1}"
    elif [ "$last" = check ]; then
      [ "${CHECK_FAIL:-0}" = 0 ] || { echo "agents/tutor.yaml: agent cs101-tutor: model.adapter: unknown adapter \"antropic\"" >&2; exit 1; }
    elif [ "$last" = up ]; then
      exit "${MIGRATE_FAIL:-0}"
    fi ;;
esac
EOF
cat > "$work/bin/runuser" <<'EOF'
#!/usr/bin/env bash
# Like pg_dump, it writes the file it is given, and a failing one leaves it.
echo "runuser $*" >> "$CALLS"
while [ $# -gt 0 ]; do [ "$1" = -f ] && { echo partial > "$2"; break; }; shift; done
exit "${BACKUP_FAIL:-0}"
EOF
cat > "$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
echo "curl $*" >> "$CALLS"
printf '%s\n' "$HEALTH_JSON"
EOF
printf '#!/bin/sh\nexit 0\n' > "$work/bin/flock"
printf '#!/bin/sh\nexit 0\n' > "$work/bin/sleep"
chmod +x "$work/bin/"*

failed=0
fail() { echo "FAIL $case: $*" >&2; failed=1; }

# setup CASE: a fresh server, the env file, and a clean record.
setup() {
  case=$1
  export STATE=$work/$case CALLS=$work/$case/calls
  rm -rf "$STATE"
  mkdir -p "$STATE/backups" "$STATE/agents" "$STATE/secrets"
  : > "$CALLS"
  printf 'DATABASE_URL=postgres://aishie_runtime:0123abcd@127.0.0.1:5432/aishie_runtime\nHTTP_ADDR=127.0.0.1:9090\nLOG_FORMAT=json\n' > "$STATE/env"
  export AISHIE_RUNTIME_ENV_FILE=$STATE/env AISHIE_RUNTIME_CONFIG_DIR=$STATE/agents \
    AISHIE_RUNTIME_SECRETS_DIR=$STATE/secrets AISHIE_RUNTIME_BACKUP_DIR=$STATE/backups \
    AISHIE_RUNTIME_LOG_FILE=$STATE/log AISHIE_RUNTIME_LOCK_FILE=$STATE/lock \
    AISHIE_RUNTIME_HEALTH_TRIES=3 AISHIE_RUNTIME_CORE_ENV_FILE=$STATE/core.env
  export VERSION_OUT="aishie-runtime v0.2.0 (abc1234, 2026-09-25T04:10:07Z)"
  export HEALTH_JSON='{"status":"ok","version":"v0.2.0","commit":"abc1234"}'
  unset PULL_FAIL CHECK_FAIL MIGRATE_FAIL BACKUP_FAIL
}
deploy() { PATH="$work/bin:$PATH" "$here/aishie-runtime-deploy" "$@" > "$STATE/out" 2>&1; }
called() { grep -q -- "$1" "$CALLS"; }
# line PATTERN: the first line of the record that matches, 0 if none.
line() { grep -n -- "$1" "$CALLS" | head -n 1 | cut -d: -f1 || true; }
# ran_nothing: the case stopped before it ran anything at all.
ran_nothing() { [ ! -s "$CALLS" ] || fail "ran something: $(paste -sd ';' "$CALLS")"; }

# Anything that is not this repository's image is refused before anything
# runs, Core's own included: each repository deploys only its own image.
for bad in "" "nginx:latest" "$REPO" "$REPO-evil:1" "ghcr.io/other/aishie-agent-runtime:1" \
  "ghcr.io/aishie-education/aishie-core:0.2.0" \
  "$IMG;id" "$IMG id" "$IMG\$(id)" "$IMG\`id\`" "$IMG|id" "$IMG&id" "$IMG'x"; do
  setup refused
  if deploy "$bad"; then fail "accepted «$bad»"; fi
  ran_nothing
done

# By digest, as the Deploy workflow calls it.
setup by-digest
deploy "$REPO@sha256:$(printf 'a%.0s' $(seq 64))" || fail "refused a digest: $(cat "$STATE/out")"

# A first deploy: check, backup, migrate, start, and wait for the new version.
setup first
deploy "$IMG" || fail "exit $?: $(cat "$STATE/out")"
for step in "docker pull $IMG" "docker run --rm $IMG version" " check$" \
  "runuser -u postgres -- pg_dump -p 5432 -Fc -f $STATE/backups/deploy-.*\.dump\.part aishie_runtime$" \
  "migrate up" "docker run -d --name aishie-runtime"; do
  called "$step" || fail "no «$step»"
done
[ "$(line ' check$')" -lt "$(line 'pg_dump')" ] || fail "backed up before checking the configuration"
[ "$(line 'pg_dump')" -lt "$(line 'migrate up')" ] || fail "migrated before the backup"
[ "$(line 'migrate up')" -lt "$(line 'docker run -d')" ] || fail "started before migrating"
[ "$(line 'docker run -d')" -lt "$(line 'docker image prune')" ] || fail "images not pruned after the deploy"
# Only this repository's images: Core's may share the server.
called "docker image prune -af --filter label=org.opencontainers.image.source=https://github.com/AIShie-Education/AIShie-Agent-Runtime$" ||
  fail "pruned $(grep 'image prune' "$CALLS")"
grep -q -- "--sig-proxy=false .* migrate up" "$CALLS" || fail "migrate up passes a Ctrl-C on: $(grep 'migrate up' "$CALLS")"
# The service and every one-off see the same things, as the nonroot user.
for what in ' check$' 'migrate up' 'docker run -d'; do
  for flag in "--network host" "--user 65532:65532" "--env-file $STATE/env" \
    "-v $STATE/agents:/config:ro -e CONFIG=/config" "-v $STATE/secrets:/secrets:ro -e SECRETS_DIR=/secrets"; do
    grep -- "$what" "$CALLS" | grep -q -- " $flag " || fail "«$what» without «$flag»: $(grep -- "$what" "$CALLS" | head -n 1)"
  done
done
start=$(grep -- 'docker run -d' "$CALLS")
for flag in "--restart unless-stopped" "--stop-timeout 30" "--log-opt max-size=20m"; do
  [[ $start == *" $flag "* ]] || fail "started without «$flag»: $start"
done
[[ $start == *" $IMG" ]] || fail "started something else: $start"
ls "$STATE/backups"/deploy-*.dump > /dev/null 2>&1 || fail "no backup kept"
! ls "$STATE/backups"/*.part > /dev/null 2>&1 || fail "a .part file left behind"
! called "docker stop" || fail "stopped a container that was not there"
[ "$(cat "$STATE/container")" = "$IMG" ] || fail "running $(cat "$STATE/container")"
called "curl .*http://127.0.0.1:9090/healthz" || fail "health checked elsewhere: $(grep '^curl' "$CALLS" | head -n 1)"
grep -q "none -> $IMG" "$STATE/log" || fail "log: $(cat "$STATE/log")"
grep -q "deployed: aishie-runtime v0.2.0 (abc1234" "$STATE/out" || fail "said: $(tail -n 1 "$STATE/out")"

# An upgrade: the old container goes only after the migration, and is given
# the time it needs to stop.
setup upgrade
echo "$OLD" > "$STATE/container"
echo 30 > "$STATE/stop-timeout"
deploy "$IMG" || fail "exit $?: $(cat "$STATE/out")"
[ "$(line 'migrate up')" -lt "$(line 'docker stop')" ] || fail "stopped the old version before migrating"
called "docker stop -t 30 aishie-runtime" || fail "stopped it: $(grep 'docker stop' "$CALLS")"
[ "$(line 'docker stop')" -lt "$(line 'docker rm')" ] || fail "removed it before stopping it"
[ "$(cat "$STATE/container")" = "$IMG" ] || fail "running $(cat "$STATE/container")"
grep -q "$OLD -> $IMG" "$STATE/log" || fail "log: $(cat "$STATE/log")"

# SHUTDOWN_GRACE, a Go duration, and 15 seconds more: the new container's stop
# timeout. The old one is given the longer of its own and that.
for pair in "1m30s=105" "45s=60" "500ms=16" "1h=3615" "2.5s=18" "soon=30" "=30"; do
  setup "grace-${pair%%=*}"
  printf 'SHUTDOWN_GRACE=%s\n' "${pair%%=*}" >> "$STATE/env"
  echo "$OLD" > "$STATE/container"
  echo 200 > "$STATE/stop-timeout"
  deploy "$IMG" || fail "exit $?: $(cat "$STATE/out")"
  called "docker run -d .* --stop-timeout ${pair#*=} " || fail "started with $(grep -o -- '--stop-timeout [0-9]*' "$CALLS")"
  want=200
  [ "${pair#*=}" -le 200 ] || want=${pair#*=}
  called "docker stop -t $want aishie-runtime" || fail "stopped with $(grep 'docker stop' "$CALLS")"
done

# A configuration the new version refuses changes nothing, and says why.
setup check-fails
echo "$OLD" > "$STATE/container"
if CHECK_FAIL=1 deploy "$IMG"; then fail "went on with a configuration the new version refuses"; fi
! called "pg_dump" || fail "backed up"
! called "migrate up" || fail "migrated"
! called "docker stop" || fail "stopped the old version"
! called "docker run -d" || fail "started the new version"
[ "$(cat "$STATE/container")" = "$OLD" ] || fail "running $(cat "$STATE/container")"
grep -q 'unknown adapter' "$STATE/out" || fail "did not show check's errors: $(cat "$STATE/out")"
grep -q "does not pass .aishie-runtime check." "$STATE/out" || fail "said: $(tail -n 1 "$STATE/out")"

# A migration that fails leaves the old version running and touched nothing.
setup migrate-fails
echo "$OLD" > "$STATE/container"
if MIGRATE_FAIL=1 deploy "$IMG"; then fail "went on after a failed migration"; fi
! called "docker stop" || fail "stopped the old version"
! called "docker run -d" || fail "started the new version"
[ "$(cat "$STATE/container")" = "$OLD" ] || fail "running $(cat "$STATE/container")"
grep -q "migrate up failed" "$STATE/out" || fail "said: $(cat "$STATE/out")"
! called "image prune" || fail "pruned after a failed deploy"

# No backup, no migration, and no half a backup left among the good ones.
setup backup-fails
echo "$OLD" > "$STATE/container"
if BACKUP_FAIL=1 deploy "$IMG"; then fail "went on without a backup"; fi
! called "migrate up" || fail "migrated without a backup"
[ -z "$(find "$STATE/backups" -type f)" ] || fail "left $(find "$STATE/backups" -type f)"

# A pull that is refused changes nothing.
setup pull-fails
echo "$OLD" > "$STATE/container"
if PULL_FAIL=1 deploy "$IMG"; then fail "went on without the image"; fi
! called "pg_dump" || fail "backed up"
grep -q "could not pull" "$STATE/out" || fail "said: $(cat "$STATE/out")"

# The database is DATABASE_URL's, on this server: its name and port.
for pair in "postgres://u:p@localhost:5433/rt_db?sslmode=disable=5433 rt_db" \
  "postgresql:///aishie_runtime=5432 aishie_runtime" \
  "postgres://u:p@[::1]:5434/aishie_rt=5434 aishie_rt" "postgres://u:p@[::1]/aishie_rt=5432 aishie_rt" \
  "postgres://u:p@127.0.0.1/aishie_runtime=5432 aishie_runtime"; do
  url=${pair%=*} want=${pair##*=}
  setup "db-${want// /-}"
  printf 'DATABASE_URL=%s\n' "$url" >> "$STATE/env"
  deploy "$IMG" || fail "exit $?: $(cat "$STATE/out")"
  called "pg_dump -p ${want% *} -Fc -f .* ${want#* }$" || fail "$url: $(grep pg_dump "$CALLS")"
done

# ...never Core's, nor one elsewhere, nor none: refused before anything runs.
for url in "postgres://aishiteru:x@127.0.0.1:5432/aishiteru" "postgres://u:p@db.example.com:5432/aishie_runtime" \
  "postgres://u:p@10.0.0.7/aishie_runtime" "postgres://u:p@127.0.0.1:5432/" "mysql://u:p@127.0.0.1/aishie_runtime" \
  "postgres://u:p@127.0.0.1:5432/x;id" "postgres://u:p@127.0.0.1:5432/-x" "postgres://u:p@127.0.0.1:port/aishie_runtime" ""; do
  setup refused-db
  printf 'DATABASE_URL=%s\n' "$url" >> "$STATE/env"
  if deploy "$IMG"; then fail "deployed with DATABASE_URL=$url"; fi
  ran_nothing
done
setup refused-core-db
printf 'DATABASE_URL=postgres://lms:x@127.0.0.1:5432/lms\n' > "$STATE/core.env"
printf 'DATABASE_URL=postgres://rt:x@127.0.0.1:5432/lms\n' >> "$STATE/env"
if deploy "$IMG"; then fail "deployed onto Core's database"; fi
ran_nothing
grep -q "Core's database" "$STATE/out" || fail "said: $(cat "$STATE/out")"
setup core-db-elsewhere
printf 'DATABASE_URL=postgres://lms:x@127.0.0.1:5432/lms\n' > "$STATE/core.env"
deploy "$IMG" || fail "refused its own database beside Core's: $(cat "$STATE/out")"
grep "pg_dump" "$CALLS" | grep -q " aishie_runtime$" || fail "backed up $(grep pg_dump "$CALLS")"

# A new version that never reports healthy is a failure, named.
setup unhealthy
HEALTH_JSON='{"status":"ok","version":"v0.1.0","commit":"0ld0ld0"}'
if deploy "$IMG"; then fail "passed while the old version answered"; fi
grep -q "did not report healthy" "$STATE/out" || fail "said: $(cat "$STATE/out")"
[ "$(grep -c '^curl' "$CALLS")" = 3 ] || fail "asked $(grep -c '^curl' "$CALLS") times, not 3"
called "docker logs --tail" || fail "did not show the failed start's log"

# Healthy is status "ok" with the new version and commit, in any order and
# spacing; not a version that only begins with it, nor a store that is down.
for json in '{"status":"unavailable","version":"v0.2.0","commit":"abc1234"}' \
  '{"status":"ok","version":"v0.2.0-rc.1","commit":"abc1234"}' \
  '{"status":"ok","version":"v0.2.0","commit":"abc1234f"}' '{"status":"ok"}' ''; do
  setup unhealthy-json
  HEALTH_JSON=$json
  if deploy "$IMG"; then fail "healthy on «$json»"; fi
done
for json in '{"commit":"abc1234","status":"ok","version":"v0.2.0"}' \
  '{ "status": "ok", "version": "v0.2.0", "commit": "abc1234" }'; do
  setup healthy-json
  HEALTH_JSON=$json
  deploy "$IMG" || fail "not healthy on «$json»: $(tail -n 1 "$STATE/out")"
done

# ...and the version before, if there was another, is started again, and
# said to be running only once it reports healthy.
setup unhealthy-rollback
echo "$OLD" > "$STATE/container"
echo "$OLD aishie-runtime v0.1.0 (0ld0ld0, 2026-09-01T00:00:00Z)" > "$STATE/versions"
HEALTH_JSON='{"status":"ok","version":"v0.1.0","commit":"0ld0ld0"}'
if deploy "$IMG"; then fail "passed while the new version was not healthy"; fi
[ "$(cat "$STATE/container")" = "$OLD" ] || fail "left $(cat "$STATE/container") running, not $OLD"
[ "$(grep -c 'docker stop' "$CALLS")" = 2 ] || fail "stopped $(grep -c 'docker stop' "$CALLS") times, not 2 (the old, then the new)"
grep -q "rolled back" "$STATE/log" || fail "log: $(cat "$STATE/log")"
grep -q "rolled back: $OLD is running again" "$STATE/out" || fail "said: $(tail -n 1 "$STATE/out")"
! called "image prune" || fail "pruned the image rolled back to"

# A version before that does not come up either (the env file or the
# configuration, most likely) is not reported as running.
setup unhealthy-rollback-too
echo "$OLD" > "$STATE/container"
echo "$OLD aishie-runtime v0.1.0 (0ld0ld0, 2026-09-01T00:00:00Z)" > "$STATE/versions"
HEALTH_JSON='{"status":"unavailable"}'
if deploy "$IMG"; then fail "passed while nothing was healthy"; fi
grep -q "did not report healthy either" "$STATE/out" || fail "said: $(tail -n 1 "$STATE/out")"

# The same image under another name (:sha- by hand, then by digest) is not a
# version to go back to.
setup unhealthy-alias
SHA_NAME=$REPO:sha-abc1234
DIGEST_NAME="$REPO@sha256:$(printf 'd%.0s' $(seq 64))"
echo "$SHA_NAME" > "$STATE/container"
printf '%s same\n%s same\n' "$SHA_NAME" "$DIGEST_NAME" > "$STATE/ids"
HEALTH_JSON='{"status":"unavailable"}'
if deploy "$DIGEST_NAME"; then fail "passed while not healthy"; fi
[ "$(grep -c 'docker run -d' "$CALLS")" = 1 ] || fail "started $(grep -c 'docker run -d' "$CALLS") containers for one image"

# The same image again (a change to the env file) has nothing to go back to.
setup unhealthy-same
echo "$IMG" > "$STATE/container"
HEALTH_JSON='{"status":"unavailable"}'
if deploy "$IMG"; then fail "passed while not healthy"; fi
[ "$(grep -c 'docker run -d' "$CALLS")" = 1 ] || fail "started $(grep -c 'docker run -d' "$CALLS") containers"

# HTTP_ADDR decides where /healthz is asked; without it, the runtime's default.
for pair in "=127.0.0.1:9090" ":9091=127.0.0.1:9091" "0.0.0.0:9092=127.0.0.1:9092" "10.0.0.5:9093=10.0.0.5:9093"; do
  setup "addr-${pair%%=*}"
  printf 'DATABASE_URL=postgres://aishie_runtime:x@127.0.0.1:5432/aishie_runtime\n' > "$STATE/env"
  [ -z "${pair%%=*}" ] || printf 'HTTP_ADDR=%s\n' "${pair%%=*}" >> "$STATE/env"
  deploy "$IMG" || fail "exit $?: $(cat "$STATE/out")"
  called "curl .*http://${pair#*=}/healthz" || fail "asked $(grep '^curl' "$CALLS" | head -n 1)"
done

# The newest ten deploy backups stay; the daily ones are not touched.
setup prune
for n in $(seq -w 1 12); do touch -t "202601${n}0000" "$STATE/backups/deploy-202601$n-000000.dump"; done
touch "$STATE/backups/daily-1.dump"
deploy "$IMG" || fail "exit $?: $(cat "$STATE/out")"
kept=$(find "$STATE/backups" -name 'deploy-*.dump' | wc -l)
[ "$kept" -eq 10 ] || fail "kept $kept deploy backups, not 10"
[ ! -e "$STATE/backups/deploy-20260101-000000.dump" ] || fail "kept the oldest"
[ -e "$STATE/backups/daily-1.dump" ] || fail "removed a daily backup"

# No env file, configuration or secrets directory: nothing runs.
for gone in env agents secrets; do
  setup "no-$gone"
  rm -r "${STATE:?}/$gone"
  if deploy "$IMG"; then fail "ran without $gone"; fi
  ran_nothing
done

[ "$failed" = 0 ] && echo "aishie-runtime-deploy: ok"
exit "$failed"
