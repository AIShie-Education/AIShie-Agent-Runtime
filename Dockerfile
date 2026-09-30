# syntax=docker/dockerfile:1

# Compile on the build machine's own architecture and cross-compile for the
# target, which keeps multi-arch builds fast (no emulation for the Go step).
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w \
      -X github.com/AIShie-Education/AIShie-Agent-Runtime/internal/version.Version=${VERSION} \
      -X github.com/AIShie-Education/AIShie-Agent-Runtime/internal/version.Commit=${COMMIT} \
      -X github.com/AIShie-Education/AIShie-Agent-Runtime/internal/version.Date=${DATE}" \
    -o /out/aishie-runtime ./cmd/aishie-runtime

# The store's migrations and the built-in prompts are embedded in the binary.
# Beside it the image holds only what the runtime's OCR runs (package ocr,
# docs/deploying.md): tesseract with its Chinese, simplified and traditional,
# and English data (Debian packages tessdata_fast's models, the small ones),
# pdftoppm (poppler-utils) to render a PDF's pages, and prlimit, which is
# util-linux's and in every Debian; what converts Office files (package
# office): LibreOffice's Impress, Writer and Calc without their windows
# (the -nogui packages), and poppler's pdftocairo, pdfseparate and pdfunite
# to cut a PDF's pages; the fonts a course's files are drawn in, Noto's CJK
# (sans and serif, simplified and traditional Chinese each in its own
# forms, with Japanese and Korean; 91 MB, where WenQuanYi's Zen Hei is 16 MB
# but draws the characters both scripts share in the mainland's forms alone,
# and lacks many beyond GBK and Big5) and the ones whose widths are Office's
# (Liberation for Arial, Times New Roman and Courier New, Carlito for
# Calibri, Caladea for Cambria), so that a deck's text stays in its boxes;
# and the CA certificates the runtime's HTTPS calls need, which distroless
# held. gpg is named only so that poppler's and LibreOffice's libraries,
# which ask for gnupg or gpg, take gpg alone: 9 MB less, no agent, dirmngr or
# translations. No recommended package is installed, and apt's lists and
# caches are removed. It runs as 65532:65532, distroless's nonroot user,
# whose uid and gid the deploy script passes, with the same home and
# working directory. Agents' configuration and secrets are mounted in
# (CONFIG, SECRETS_DIR).
FROM debian:13-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates gpg poppler-utils \
      tesseract-ocr tesseract-ocr-chi-sim tesseract-ocr-chi-tra tesseract-ocr-eng \
      libreoffice-impress-nogui libreoffice-writer-nogui libreoffice-calc-nogui \
      fonts-noto-cjk fonts-liberation fonts-crosextra-carlito fonts-crosextra-caladea \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* /var/cache/debconf/*-old /var/log/apt /var/log/dpkg.log \
 && groupadd --gid 65532 nonroot \
 && useradd --uid 65532 --gid 65532 --home-dir /home/nonroot --create-home --shell /usr/sbin/nologin nonroot
COPY --from=build /out/aishie-runtime /usr/local/bin/aishie-runtime
USER 65532:65532
WORKDIR /home/nonroot
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/aishie-runtime"]
CMD ["run"]
