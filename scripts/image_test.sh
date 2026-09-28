#!/usr/bin/env bash
# image_test.sh [IMAGE]: the runtime's image (aishie-runtime:dev, as `make
# docker` builds it, when none is named) runs as the deploy script runs it,
# and its OCR reads a scanned page for real.
#
#   make docker-test
#
# It checks the image's user (65532:65532, distroless's nonroot, whose uid
# and gid aishie-runtime-deploy passes) and entrypoint; that `check` with
# OCR=on passes as that user with no network, which it does only when
# tesseract with its Chinese and English data, pdftoppm and prlimit are
# there; and it runs the ocr package's tests against the programs in the
# image (OCR_REQUIRED=1: none is skipped), as that user, with no network.
set -euo pipefail

img=${1:-aishie-runtime:dev}
here=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail() { echo "image_test: $*" >&2; exit 1; }

got=$(docker image inspect --format '{{.Config.User}} {{json .Config.Entrypoint}} {{json .Config.Cmd}}' "$img")
[ "$got" = '65532:65532 ["/usr/local/bin/aishie-runtime"] ["run"]' ] ||
  fail "$img runs as, with and by: $got"
docker run --rm "$img" version

out=$(docker run --rm --user 65532:65532 --network none -e OCR=on \
  -v "$here/examples":/config:ro -e CONFIG=/config/runtime.yaml,/config/agents "$img" check 2>&1) ||
  fail "check with OCR=on did not pass: $out"
grep -q '^ocr: tesseract .* chi_sim+chi_tra+eng ' <<<"$out" || fail "check does not say OCR runs: $out"
grep '^ocr: ' <<<"$out"

# The test binary is static, as the runtime is, and runs in the image
# as it is.
(cd "$here" && CGO_ENABLED=0 go test -c -o "$work/ocr.test" ./internal/ocr)
chmod 0755 "$work" "$work/ocr.test"
docker run --rm --user 65532:65532 --network none -e OCR_REQUIRED=1 \
  -v "$work/ocr.test":/usr/local/bin/ocr.test:ro --entrypoint /usr/local/bin/ocr.test "$img" \
  -test.run 'TestRecognize' -test.v
echo "image_test: $img passes"
