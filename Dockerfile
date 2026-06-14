# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/busalertbot ./cmd/busalertbot

FROM alpine:3.23

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S bot \
    && adduser -S -G bot bot
WORKDIR /app
COPY --from=build /out/busalertbot /usr/local/bin/busalertbot
RUN mkdir -p /app/data && chown -R bot:bot /app
USER bot
VOLUME ["/app/data"]
ENTRYPOINT ["busalertbot"]
