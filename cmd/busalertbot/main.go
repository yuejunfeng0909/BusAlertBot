package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"busalertbot/internal/bot"
	"busalertbot/internal/config"
	"busalertbot/internal/lta"
	"busalertbot/internal/store"
	"busalertbot/internal/telegram"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	location, _ := time.LoadLocation(cfg.Timezone)
	data, err := store.Open(cfg.DataFile)
	if err != nil {
		log.Error("open data store", "error", err)
		os.Exit(1)
	}
	defer data.Close()

	app := bot.New(
		log,
		data,
		lta.New(cfg.LTAAccountKey),
		telegram.New(cfg.TelegramToken, cfg.PollTimeout),
		location,
		cfg.PollTimeout,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx); err != nil {
		log.Error("bot stopped", "error", err)
		os.Exit(1)
	}
}
