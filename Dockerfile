FROM node:22-alpine AS web-builder
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.24-alpine AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/newnexus-server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/newnexus-agent ./cmd/agent

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && addgroup -S newnexus && adduser -S -G newnexus newnexus
WORKDIR /app
COPY --from=go-builder /out/newnexus-server ./newnexus-server
COPY --from=go-builder /out/newnexus-agent ./newnexus-agent
COPY --from=web-builder /src/web/dist ./web/dist
COPY api ./api
RUN mkdir -p /data && chown -R newnexus:newnexus /app /data
USER newnexus
EXPOSE 8080 9090
VOLUME ["/data"]
ENTRYPOINT ["/app/newnexus-server"]
