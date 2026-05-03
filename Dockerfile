# syntax=docker/dockerfile:1

FROM node:22-alpine AS web-builder
WORKDIR /app/web

COPY web/package*.json ./
RUN npm ci

COPY web/ ./
RUN npm run build

FROM golang:1.25-alpine AS go-builder
WORKDIR /app

COPY go.mod go.sum ./
COPY pkg/ ./pkg/
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY web/ ./web/
COPY --from=web-builder /app/web/dist ./web/dist

ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/torrent-stream-hub ./cmd/hub

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && \
    mkdir -p /downloads /config
WORKDIR /app

COPY --from=go-builder /out/torrent-stream-hub /usr/local/bin/torrent-stream-hub

EXPOSE 8080 50007/tcp 50007/udp

ENTRYPOINT ["torrent-stream-hub"]
