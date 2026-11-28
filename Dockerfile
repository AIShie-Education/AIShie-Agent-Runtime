# syntax=docker/dockerfile:1

# Cross-compile the Go control plane without emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.4 AS go-build
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

# Build the TypeScript runner on Node 22, then remove development-only
# packages. Production dependencies stay beside dist in the final image.
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS runner-build
WORKDIR /src/runner
COPY runner/package.json runner/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY runner/ ./
RUN npm run typecheck && npm run build && npm prune --omit=dev

# One small image, one process tree: aishie-runtime starts and supervises the
# loopback Node runner. No LibreOffice, Tesseract, Poppler or CJK font bundle.
FROM node:22-bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* /var/cache/debconf/*-old /var/log/apt /var/log/dpkg.log \
 && groupadd --gid 65532 nonroot \
 && useradd --uid 65532 --gid 65532 --home-dir /home/nonroot --create-home --shell /usr/sbin/nologin nonroot
COPY --from=go-build /out/aishie-runtime /usr/local/bin/aishie-runtime
COPY --from=runner-build /src/runner/dist /opt/aishie/runner/dist
COPY --from=runner-build /src/runner/node_modules /opt/aishie/runner/node_modules
COPY --from=runner-build /src/runner/package.json /opt/aishie/runner/package.json
ENV RUNNER_ENTRY=/opt/aishie/runner/dist/index.js
USER 65532:65532
WORKDIR /home/nonroot
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/aishie-runtime"]
CMD ["run"]
