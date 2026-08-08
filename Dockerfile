FROM --platform=$BUILDPLATFORM node:22-alpine AS web-builder
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/newnexus-server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/newnexus-agent-linux-amd64 ./cmd/agent
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/newnexus-agent-linux-arm64 ./cmd/agent

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && addgroup -S newnexus && adduser -S -G newnexus newnexus
WORKDIR /app
COPY --from=go-builder /out/newnexus-server ./newnexus-server
COPY --from=go-builder /out/newnexus-agent-linux-amd64 ./newnexus-agent-linux-amd64
COPY --from=go-builder /out/newnexus-agent-linux-arm64 ./newnexus-agent-linux-arm64
COPY --from=web-builder /src/web/dist ./web/dist
COPY api ./api
COPY deploy ./deploy
RUN mkdir -p /data && chown -R newnexus:newnexus /app /data
USER newnexus
EXPOSE 8080 9090
VOLUME ["/data"]
ARG VERSION=dev
ENV NEWNEXUS_VERSION=${VERSION} \
    NEWNEXUS_AGENT_BINARY_PATH=/app/newnexus-agent-linux-{arch}
ENTRYPOINT ["/app/newnexus-server"]
