# Build from repository root: docker build -f cmd/agent/Dockerfile -t newnexus-agent .
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=0.1.0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/agentproto ./internal/agentproto
COPY internal/agentclient ./internal/agentclient
COPY cmd/agent ./cmd/agent
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X github.com/newnexus/newnexus/internal/agentclient.Version=$VERSION" -o /agent ./cmd/agent

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* && groupadd -g 65532 agent && useradd -r -u 65532 -g 65532 agent && mkdir /data && chown 65532:65532 /data
COPY --from=build /agent /usr/local/bin/newnexus-agent
USER 65532:65532
ENV NEWNEXUS_AGENT_DATA_DIR=/data
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 CMD ["/usr/local/bin/newnexus-agent", "--healthcheck"]
ENTRYPOINT ["/usr/local/bin/newnexus-agent"]
