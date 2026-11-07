FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --registry=https://registry.npmjs.org --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/server ./cmd/server
COPY cmd/agent ./cmd/agent
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /newnexus-server ./cmd/server
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build mkdir /agent-downloads && \
    for architecture in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" go build -trimpath -ldflags='-s -w' -o "/agent-downloads/newnexus-agent-linux-$architecture" ./cmd/agent || exit 1; \
    done

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* && groupadd -g 65532 newnexus && useradd -r -u 65532 -g 65532 newnexus && mkdir /data /app && chown 65532:65532 /data
WORKDIR /app
COPY --from=build /newnexus-server /usr/local/bin/newnexus-server
COPY --from=web /src/web/dist /app/web
COPY --from=build /agent-downloads /app/web/agent
COPY api/openapi.yaml /app/api/openapi.yaml
COPY LICENSE /app/LICENSE
USER 65532:65532
ENV NEWNEXUS_ENV=production NEWNEXUS_DATA_DIR=/data NEWNEXUS_WEB_DIR=/app/web NEWNEXUS_HTTP_ADDR=0.0.0.0:8080 NEWNEXUS_GRPC_ADDR=0.0.0.0:9090
EXPOSE 8080 9090
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 CMD ["/usr/local/bin/newnexus-server", "--healthcheck"]
ENTRYPOINT ["/usr/local/bin/newnexus-server"]
