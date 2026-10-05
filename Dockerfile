# syntax=docker/dockerfile:1

# 1. The pane, built once on the builder's platform.
FROM --platform=$BUILDPLATFORM oven/bun:1.4 AS web
WORKDIR /src/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
RUN bun run build

# 2. A static binary, cross-compiled without QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26 AS go
ARG TARGETOS TARGETARCH VERSION=dev COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=web /src/web/dist/client ./web/dist/client
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT" \
      -o /out/embolt ./cmd/embolt

# 3. Distroless: CA certificates and tzdata, no shell.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=go /out/embolt /usr/local/bin/embolt
COPY LICENSE THIRD_PARTY_NOTICES /licenses/
ENV GOMEMLIMIT=800MiB
VOLUME ["/config", "/data"]
EXPOSE 8096 8920 9090
HEALTHCHECK --interval=30s --timeout=3s CMD ["/usr/local/bin/embolt", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/embolt"]
CMD ["serve", "--config", "/config/config.yaml"]
