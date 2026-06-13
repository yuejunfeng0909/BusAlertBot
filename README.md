# BusAlertBot

A Telegram bot for Singapore LTA bus arrival watches. Users can keep a
per-chat watchlist of bus stop and service pairs, start one-minute ETA updates,
and schedule a daily 15-minute notification session.

## Features

- Search bus stops by name, road, or five-digit stop code.
- Add and delete watch items with stable, auto-incrementing IDs.
- Show all watches and daily schedules with `/watchlist`.
- Send ETA updates every minute for 15 minutes.
- Extend any active session by another 15 minutes with an inline button.
- Dismiss an active session with an inline button.
- Deliver normal ETA updates silently.
- Enable Telegram notification sound when the next ETA is under two minutes.
- Persist watchlists and schedules in an atomically-written JSON file.

## Requirements

- Go 1.26 or newer.
- A Telegram bot token from [BotFather](https://t.me/BotFather).
- An LTA DataMall account key from the
  [LTA DataMall portal](https://datamall.lta.gov.sg/content/datamall/en/request-for-api.html).

Go 1.27 is not yet a stable release as of June 13, 2026. This project targets
the current stable language version, Go 1.26, and uses only the standard
library so upgrading the `go` directive when 1.27 ships should be routine.

## Configure

```sh
cp .env.example .env
```

Set `TELEGRAM_BOT_TOKEN` and `LTA_ACCOUNT_KEY` in `.env`. Environment variables
are not loaded automatically by the binary; use your service manager, shell,
or Docker Compose to provide them.

## Run

With Docker Compose:

```sh
cp .env.example .env
# Set TELEGRAM_BOT_TOKEN and LTA_ACCOUNT_KEY in .env.
docker compose up -d
```

The included `compose.yaml` pulls the latest published image from GHCR and
stores bot state in the Docker-managed `busalertbot-data` volume. The `.env`
file must be in the same directory as `compose.yaml`.

To inspect the service:

```sh
docker compose logs -f
```

Directly:

```sh
set -a
. ./.env
set +a
go run ./cmd/busalertbot
```

State is stored in `/app/data/state.json` in the container by default. The
Compose deployment keeps it in a persistent named volume.

## CI/CD

Every push to `main` runs the tests and vet checks, then publishes a
multi-platform image for `linux/amd64` and `linux/arm64` to GitHub Container
Registry:

```text
ghcr.io/yuejunfeng0909/busalertbot:latest
ghcr.io/yuejunfeng0909/busalertbot:<full-commit-sha>
```

The workflow uses the repository's built-in `GITHUB_TOKEN`; no registry secret
is required. New GHCR packages may initially be private. Change the package
visibility in the repository owner's GitHub package settings if anonymous
pulls are required.

## Commands

```text
/find <name>
/add <stop name or code> | <service>
/watchlist
/delete <ID>
/notify <ID>
/schedule <ID> <HH:MM>
/unschedule <ID>
/help
```

Example:

```text
/find Raffles Hotel
/add 02049 | 36
/notify 1
/schedule 1 07:30
```

Daily times use `TIMEZONE`, which defaults to `Asia/Singapore`. A daily
schedule starts an ETA session immediately at the configured time and repeats
once per minute for 15 minutes. Each ETA message provides controls to approve
another 15 minutes or dismiss the session.

## Verify

```sh
go test ./...
go vet ./...
```

## Data sources

The implementation follows LTA DataMall API User Guide 6.8:

- `BusStops` for stop lookup.
- `v3/BusArrival` for real-time ETA data.

Telegram integration uses HTTPS long polling through the official Bot API.
