package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"busalertbot/internal/lta"
	"busalertbot/internal/store"
	"busalertbot/internal/telegram"
)

const sessionDuration = 15 * time.Minute

type LTAClient interface {
	SearchStops(context.Context, string, int) ([]lta.BusStop, error)
	Arrivals(context.Context, string, string) ([]lta.ServiceArrival, error)
}

type TelegramClient interface {
	GetUpdates(context.Context, int64, time.Duration) ([]telegram.Update, error)
	SendMessage(context.Context, int64, string, bool, *telegram.InlineKeyboardMarkup) error
	AnswerCallback(context.Context, string, string) error
	SetCommands(context.Context, []telegram.BotCommand) error
}

type Bot struct {
	log         *slog.Logger
	store       *store.Store
	lta         LTAClient
	telegram    TelegramClient
	location    *time.Location
	pollTimeout time.Duration

	sessionsMu sync.Mutex
	sessions   map[sessionKey]session
}

type sessionKey struct {
	chatID  int64
	watchID int
}

type session struct {
	expiresAt time.Time
	nextAt    time.Time
}

func New(log *slog.Logger, data *store.Store, ltaClient LTAClient, telegramClient TelegramClient, location *time.Location, pollTimeout time.Duration) *Bot {
	return &Bot{
		log:         log,
		store:       data,
		lta:         ltaClient,
		telegram:    telegramClient,
		location:    location,
		pollTimeout: pollTimeout,
		sessions:    make(map[sessionKey]session),
	}
}

func (b *Bot) Run(ctx context.Context) error {
	if err := b.registerCommands(ctx); err != nil {
		b.log.Warn("could not register Telegram commands", "error", err)
	}

	go b.runScheduler(ctx)

	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		updates, err := b.telegram.GetUpdates(ctx, offset, b.pollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			b.log.Error("Telegram polling failed", "error", err)
			if !sleepContext(ctx, 2*time.Second) {
				return nil
			}
			continue
		}
		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			if update.Message != nil && update.Message.Text != "" {
				b.handleMessage(ctx, update.Message)
			}
			if update.CallbackQuery != nil {
				b.handleCallback(ctx, update.CallbackQuery)
			}
		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, message *telegram.Message) {
	command, args := splitCommand(message.Text)
	chatID := message.Chat.ID
	switch command {
	case "/start", "/help":
		b.send(ctx, chatID, helpText, false, nil)
	case "/add":
		b.handleAdd(ctx, chatID, args)
	case "/find":
		b.handleFind(ctx, chatID, args)
	case "/watchlist", "/list":
		b.handleWatchlist(ctx, chatID)
	case "/delete":
		b.handleDelete(ctx, chatID, args)
	case "/notify":
		b.handleNotify(ctx, chatID, args)
	case "/schedule":
		b.handleSchedule(ctx, chatID, args)
	case "/unschedule":
		b.handleUnschedule(ctx, chatID, args)
	default:
		b.send(ctx, chatID, "Unknown command. Use /help to see the available commands.", false, nil)
	}
}

func (b *Bot) handleAdd(ctx context.Context, chatID int64, args string) {
	stopQuery, serviceNo, ok := parseAddArgs(args)
	if !ok {
		b.send(ctx, chatID, "Usage: /add <bus stop name or 5-digit code> | <service>\nExample: /add Raffles Hotel | 36", false, nil)
		return
	}
	stops, err := b.lta.SearchStops(ctx, stopQuery, 6)
	if err != nil {
		b.fail(chatID, "search bus stops", err)
		return
	}
	if len(stops) == 0 {
		b.send(ctx, chatID, "No bus stop matched that name or code. Try /find <name>.", false, nil)
		return
	}
	if len(stops) > 1 && !strings.EqualFold(stops[0].Description, stopQuery) && stops[0].BusStopCode != stopQuery {
		b.send(ctx, chatID, formatStopMatches(stops), false, nil)
		return
	}
	stop := stops[0]
	watch, err := b.store.Add(chatID, store.Watch{
		StopCode:  stop.BusStopCode,
		StopName:  stop.Description,
		RoadName:  stop.RoadName,
		ServiceNo: serviceNo,
	})
	if errors.Is(err, store.ErrDuplicate) {
		b.send(ctx, chatID, "That stop and service are already on your watchlist.", false, nil)
		return
	}
	if err != nil {
		b.fail(chatID, "save watch item", err)
		return
	}
	b.send(ctx, chatID, fmt.Sprintf("Added #%d: bus %s at %s (%s).", watch.ID, watch.ServiceNo, watch.StopName, watch.StopCode), false, nil)
}

func (b *Bot) handleFind(ctx context.Context, chatID int64, args string) {
	if strings.TrimSpace(args) == "" {
		b.send(ctx, chatID, "Usage: /find <bus stop name or road>", false, nil)
		return
	}
	stops, err := b.lta.SearchStops(ctx, args, 10)
	if err != nil {
		b.fail(chatID, "search bus stops", err)
		return
	}
	if len(stops) == 0 {
		b.send(ctx, chatID, "No matching bus stops found.", false, nil)
		return
	}
	b.send(ctx, chatID, formatStopMatches(stops), false, nil)
}

func (b *Bot) handleWatchlist(ctx context.Context, chatID int64) {
	watches := b.store.List(chatID)
	if len(watches) == 0 {
		b.send(ctx, chatID, "Your watchlist is empty. Add one with /add <stop> | <service>.", false, nil)
		return
	}
	var text strings.Builder
	text.WriteString("Your watchlist:\n")
	for _, watch := range watches {
		fmt.Fprintf(&text, "\n#%d  Bus %s\n%s (%s)", watch.ID, watch.ServiceNo, watch.StopName, watch.StopCode)
		if watch.Schedule != "" {
			fmt.Fprintf(&text, "\nDaily: %s", watch.Schedule)
		}
		text.WriteByte('\n')
	}
	text.WriteString("\nUse /notify <ID>, /schedule <ID> HH:MM, or /delete <ID>.")
	b.send(ctx, chatID, text.String(), false, nil)
}

func (b *Bot) handleDelete(ctx context.Context, chatID int64, args string) {
	id, ok := parseID(args)
	if !ok {
		b.send(ctx, chatID, "Usage: /delete <watch ID>", false, nil)
		return
	}
	if err := b.store.Delete(chatID, id); errors.Is(err, store.ErrNotFound) {
		b.send(ctx, chatID, "No watch item has that ID.", false, nil)
		return
	} else if err != nil {
		b.fail(chatID, "delete watch item", err)
		return
	}
	b.dismiss(chatID, id)
	b.send(ctx, chatID, fmt.Sprintf("Deleted watch #%d.", id), false, nil)
}

func (b *Bot) handleNotify(ctx context.Context, chatID int64, args string) {
	id, ok := parseID(args)
	if !ok {
		b.send(ctx, chatID, "Usage: /notify <watch ID>", false, nil)
		return
	}
	watch, err := b.store.Get(chatID, id)
	if errors.Is(err, store.ErrNotFound) {
		b.send(ctx, chatID, "No watch item has that ID.", false, nil)
		return
	}
	if err != nil {
		b.fail(chatID, "load watch item", err)
		return
	}
	b.activate(chatID, id, time.Now())
	b.sendETA(ctx, chatID, watch)
}

func (b *Bot) handleSchedule(ctx context.Context, chatID int64, args string) {
	fields := strings.Fields(args)
	if len(fields) != 2 {
		b.send(ctx, chatID, "Usage: /schedule <watch ID> <HH:MM>\nExample: /schedule 2 07:30", false, nil)
		return
	}
	id, ok := parseID(fields[0])
	if !ok {
		b.send(ctx, chatID, "The watch ID must be a positive number.", false, nil)
		return
	}
	parsed, err := time.Parse("15:04", fields[1])
	if err != nil {
		b.send(ctx, chatID, "Time must use 24-hour HH:MM format, for example 07:30.", false, nil)
		return
	}
	schedule := parsed.Format("15:04")
	watch, err := b.store.SetSchedule(chatID, id, schedule)
	if errors.Is(err, store.ErrNotFound) {
		b.send(ctx, chatID, "No watch item has that ID.", false, nil)
		return
	}
	if err != nil {
		b.fail(chatID, "save schedule", err)
		return
	}
	b.send(ctx, chatID, fmt.Sprintf("Watch #%d will start a 15-minute ETA session daily at %s (%s).", watch.ID, schedule, b.location.String()), false, nil)
}

func (b *Bot) handleUnschedule(ctx context.Context, chatID int64, args string) {
	id, ok := parseID(args)
	if !ok {
		b.send(ctx, chatID, "Usage: /unschedule <watch ID>", false, nil)
		return
	}
	_, err := b.store.SetSchedule(chatID, id, "")
	if errors.Is(err, store.ErrNotFound) {
		b.send(ctx, chatID, "No watch item has that ID.", false, nil)
		return
	}
	if err != nil {
		b.fail(chatID, "remove schedule", err)
		return
	}
	b.send(ctx, chatID, fmt.Sprintf("Removed the daily schedule from watch #%d.", id), false, nil)
}

func (b *Bot) handleCallback(ctx context.Context, callback *telegram.CallbackQuery) {
	if callback.Message == nil {
		b.answer(ctx, callback.ID, "This action is no longer available.")
		return
	}
	parts := strings.SplitN(callback.Data, ":", 2)
	if len(parts) != 2 {
		b.answer(ctx, callback.ID, "Invalid action.")
		return
	}
	id, err := strconv.Atoi(parts[1])
	if err != nil || id < 1 {
		b.answer(ctx, callback.ID, "Invalid watch ID.")
		return
	}
	chatID := callback.Message.Chat.ID
	if _, err := b.store.Get(chatID, id); err != nil {
		b.answer(ctx, callback.ID, "That watch item no longer exists.")
		return
	}
	switch parts[0] {
	case "continue":
		b.activate(chatID, id, time.Now())
		b.answer(ctx, callback.ID, "ETA updates approved for 15 more minutes.")
	case "dismiss":
		b.dismiss(chatID, id)
		b.answer(ctx, callback.ID, "ETA updates dismissed.")
	default:
		b.answer(ctx, callback.ID, "Invalid action.")
	}
}

func (b *Bot) runScheduler(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			localNow := now.In(b.location)
			dueSchedules, err := b.store.ClaimDue(localNow)
			if err != nil {
				b.log.Error("claim daily schedules", "error", err)
			}
			for _, due := range dueSchedules {
				b.activate(due.ChatID, due.Watch.ID, now)
				b.sendETA(ctx, due.ChatID, due.Watch)
			}
			for _, due := range b.dueSessions(now) {
				watch, err := b.store.Get(due.chatID, due.watchID)
				if errors.Is(err, store.ErrNotFound) {
					b.dismiss(due.chatID, due.watchID)
					continue
				}
				if err != nil {
					b.log.Error("load scheduled watch", "error", err)
					continue
				}
				b.sendETA(ctx, due.chatID, watch)
			}
		}
	}
}

func (b *Bot) sendETA(ctx context.Context, chatID int64, watch store.Watch) {
	arrivals, err := b.lta.Arrivals(ctx, watch.StopCode, watch.ServiceNo)
	if err != nil {
		b.log.Error("fetch bus arrivals", "chat_id", chatID, "watch_id", watch.ID, "error", err)
		b.send(ctx, chatID, fmt.Sprintf("Watch #%d: ETA is temporarily unavailable.", watch.ID), true, sessionKeyboard(watch.ID))
		return
	}
	now := time.Now()
	text, urgent := formatETA(watch, arrivals, now)
	b.send(ctx, chatID, text, !urgent, sessionKeyboard(watch.ID))
}

func (b *Bot) activate(chatID int64, watchID int, now time.Time) {
	b.sessionsMu.Lock()
	defer b.sessionsMu.Unlock()
	b.sessions[sessionKey{chatID: chatID, watchID: watchID}] = session{
		expiresAt: now.Add(sessionDuration),
		nextAt:    now.Add(time.Minute),
	}
}

func (b *Bot) dismiss(chatID int64, watchID int) {
	b.sessionsMu.Lock()
	defer b.sessionsMu.Unlock()
	delete(b.sessions, sessionKey{chatID: chatID, watchID: watchID})
}

func (b *Bot) dueSessions(now time.Time) []sessionKey {
	b.sessionsMu.Lock()
	defer b.sessionsMu.Unlock()

	var due []sessionKey
	for key, active := range b.sessions {
		if !now.Before(active.expiresAt) {
			delete(b.sessions, key)
			continue
		}
		if now.Before(active.nextAt) {
			continue
		}
		due = append(due, key)
		active.nextAt = now.Add(time.Minute)
		b.sessions[key] = active
	}
	return due
}

func (b *Bot) registerCommands(ctx context.Context) error {
	return b.telegram.SetCommands(ctx, []telegram.BotCommand{
		{Command: "add", Description: "Add a stop and service"},
		{Command: "find", Description: "Find a bus stop code"},
		{Command: "watchlist", Description: "Show your watchlist"},
		{Command: "delete", Description: "Delete a watch by ID"},
		{Command: "notify", Description: "Start 15 minutes of ETA updates"},
		{Command: "schedule", Description: "Schedule a daily ETA session"},
		{Command: "unschedule", Description: "Remove a daily schedule"},
		{Command: "help", Description: "Show help"},
	})
}

func (b *Bot) send(ctx context.Context, chatID int64, text string, silent bool, keyboard *telegram.InlineKeyboardMarkup) {
	if err := b.telegram.SendMessage(ctx, chatID, text, silent, keyboard); err != nil {
		b.log.Error("send Telegram message", "chat_id", chatID, "error", err)
	}
}

func (b *Bot) answer(ctx context.Context, callbackID, text string) {
	if err := b.telegram.AnswerCallback(ctx, callbackID, text); err != nil {
		b.log.Error("answer Telegram callback", "error", err)
	}
}

func (b *Bot) fail(chatID int64, operation string, err error) {
	b.log.Error(operation, "chat_id", chatID, "error", err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b.send(ctx, chatID, "Something went wrong. Please try again shortly.", false, nil)
}

func splitCommand(text string) (string, string) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return "", ""
	}
	command := strings.ToLower(strings.SplitN(fields[0], "@", 2)[0])
	args := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), fields[0]))
	return command, args
}

func parseAddArgs(args string) (string, string, bool) {
	parts := strings.SplitN(args, "|", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	stop := strings.TrimSpace(parts[0])
	service := strings.ToUpper(strings.TrimSpace(parts[1]))
	if stop == "" || !validServiceNo(service) {
		return "", "", false
	}
	return stop, service, true
}

func validServiceNo(service string) bool {
	if len(service) < 1 || len(service) > 8 {
		return false
	}
	for _, r := range service {
		if (r < '0' || r > '9') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

func parseID(value string) (int, bool) {
	id, err := strconv.Atoi(strings.TrimSpace(value))
	return id, err == nil && id > 0
}

func formatStopMatches(stops []lta.BusStop) string {
	var text strings.Builder
	text.WriteString("Matching bus stops:\n")
	for _, stop := range stops {
		fmt.Fprintf(&text, "\n%s - %s, %s", stop.BusStopCode, stop.Description, stop.RoadName)
	}
	text.WriteString("\n\nAdd one with /add <code> | <service>.")
	return text.String()
}

func formatETA(watch store.Watch, services []lta.ServiceArrival, now time.Time) (string, bool) {
	var selected *lta.ServiceArrival
	for i := range services {
		if strings.EqualFold(services[i].ServiceNo, watch.ServiceNo) {
			selected = &services[i]
			break
		}
	}
	header := fmt.Sprintf("Watch #%d: bus %s at %s (%s)", watch.ID, watch.ServiceNo, watch.StopName, watch.StopCode)
	if selected == nil {
		return header + "\nNo ETA available.", false
	}

	buses := []lta.Arrival{selected.NextBus, selected.NextBus2, selected.NextBus3}
	var labels []string
	urgent := false
	for _, bus := range buses {
		if bus.EstimatedArrival == "" {
			continue
		}
		arrivalTime, err := time.Parse(time.RFC3339, bus.EstimatedArrival)
		if err != nil {
			continue
		}
		remaining := arrivalTime.Sub(now)
		if remaining < 2*time.Minute {
			urgent = true
		}
		label := durationLabel(remaining)
		if load := loadLabel(bus.Load); load != "" {
			label += " (" + load + ")"
		}
		labels = append(labels, label)
	}
	if len(labels) == 0 {
		return header + "\nNo ETA available.", false
	}
	return header + "\nETA: " + strings.Join(labels, ", "), urgent
}

func durationLabel(duration time.Duration) string {
	if duration < time.Minute {
		return "Arr"
	}
	return fmt.Sprintf("%d min", int(duration/time.Minute))
}

func loadLabel(load string) string {
	switch load {
	case "SEA":
		return "seats"
	case "SDA":
		return "standing"
	case "LSD":
		return "limited"
	default:
		return ""
	}
}

func sessionKeyboard(watchID int) *telegram.InlineKeyboardMarkup {
	return &telegram.InlineKeyboardMarkup{
		InlineKeyboard: [][]telegram.InlineKeyboardButton{{
			{Text: "Continue 15 min", CallbackData: fmt.Sprintf("continue:%d", watchID)},
			{Text: "Dismiss", CallbackData: fmt.Sprintf("dismiss:%d", watchID)},
		}},
	}
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

const helpText = `Bus ETA watchlist

/find <name> - find bus stop codes
/add <stop name or code> | <service> - add a watch
/watchlist - list watches and IDs
/delete <ID> - delete a watch
/notify <ID> - start ETA updates every minute for 15 minutes
/schedule <ID> <HH:MM> - start a 15-minute session daily
/unschedule <ID> - remove a daily schedule

Each ETA message includes buttons to approve another 15 minutes or dismiss updates. Notifications are silent unless the next bus is less than 2 minutes away.`
