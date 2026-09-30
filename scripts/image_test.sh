#!/usr/bin/env bash
# image_test.sh [IMAGE]: the runtime's image (aishie-runtime:dev, as `make
# docker` builds it, when none is named) runs as the deploy script runs it,
# its OCR reads a scanned page for real, and its LibreOffice converts decks
# and documents for real.
#
#   make docker-test
#
# It checks the image's user (65532:65532, distroless's nonroot, whose uid
# and gid aishie-runtime-deploy passes) and entrypoint; that `check` with
# OCR=on and OFFICE_PDF=on passes as that user with no network, which it
# does only when tesseract with its Chinese and English data, pdftoppm,
# LibreOffice and prlimit are there; and it runs the tests of the ocr and
# office packages, and the toolset's that convert a deck and read its
# pictures by OCR, against the programs in the image (OCR_REQUIRED=1,
# OFFICE_PDF_REQUIRED=1: none is skipped), as that user, with no network.
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

out=$(docker run --rm --user 65532:65532 --network none -e OCR=on -e OFFICE_PDF=on \
  -v "$here/examples":/config:ro -e CONFIG=/config/runtime.yaml,/config/agents "$img" check 2>&1) ||
  fail "check with OCR=on and OFFICE_PDF=on did not pass: $out"
grep -q '^ocr: tesseract .* chi_sim+chi_tra+eng ' <<<"$out" || fail "check does not say OCR runs: $out"
grep -q '^office: LibreOffice ' <<<"$out" || fail "check does not say LibreOffice converts: $out"
grep -q '^pdf parts: ' <<<"$out" || fail "check does not say PDFs are cut into parts: $out"
grep -E '^(ocr|office|pdf parts): ' <<<"$out"

# The test binaries are static, as the runtime is, and run in the image as
# they are; the toolset's reads Core's catalogue from the tree, mounted.
for pkg in ocr office toolset; do
  (cd "$here" && CGO_ENABLED=0 go test -c -o "$work/$pkg.test" "./internal/$pkg")
done
chmod 0755 "$work" "$work"/*.test
in_image() {
  docker run --rm --user 65532:65532 --network none -e OCR_REQUIRED=1 -e OFFICE_PDF_REQUIRED=1 \
    -v "$work":/tests:ro -v "$here/internal":/src/internal:ro "$@"
}
in_image --entrypoint /tests/ocr.test "$img" -test.run 'TestRecognize' -test.v
in_image --entrypoint /tests/office.test "$img" -test.run 'TestConvertReal' -test.v
in_image -w /src/internal/toolset --entrypoint /tests/toolset.test "$img" -test.run 'TestReal' -test.v
echo "image_test: $img passes"
