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
      -X github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/version.Version=${VERSION} \
      -X github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/version.Commit=${COMMIT} \
      -X github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/version.Date=${DATE}" \
    -o /out/aishie-runtime ./cmd/aishie-runtime

# The store's migrations and the built-in prompts are embedded in the binary,
# so the image is just the binary: no shell, no package manager, not root.
# Agents' configuration and secrets are mounted in (CONFIG, SECRETS_DIR).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/aishie-runtime /usr/local/bin/aishie-runtime
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/aishie-runtime"]
CMD ["run"]
