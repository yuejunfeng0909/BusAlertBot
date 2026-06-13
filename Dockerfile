FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/busalertbot ./cmd/busalertbot

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
