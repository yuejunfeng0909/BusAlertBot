package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"busalertbot/internal/lta"
	"busalertbot/internal/store"
	"busalertbot/internal/telegram"
)

const (
	addUsage           = "Usage: /add <stop[, stop...]> ; <service[, service...]>\nExample: /add 02049, 04167 ; 36, 111"
	emptyWatchlistHelp = "Your watchlist is empty. Add one with /add <stop> ; <service>."
	sessionDuration    = 15 * time.Minute
	sessionExpiryGrace = 10 * time.Second
	schedulerWorkers   = 8
	schedulerQueueSize = 512
)

type LTAClient interface {
	SearchStops(context.Context, string, int) ([]lta.BusStop, error)
	Arrivals(context.Context, string, string) ([]lta.ServiceArrival, error)
	ServicesAtStops(context.Context, []string) (map[string][]string, error)
}

type TelegramClient interface {
	GetUpdates(context.Context, int64, time.Duration) ([]telegram.Update, error)
	SendMessage(context.Context, int64, string, bool, *telegram.InlineKeyboardMarkup) error
	AnswerCallback(context.Context, string, string) error
	EditMessageReplyMarkup(context.Context, int64, int64, *telegram.InlineKeyboardMarkup) error
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

type notificationJob struct {
	chatID int64
	watch  store.Watch
	active bool
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
	case "/watchlist", "/list":
		b.handleWatchlist(ctx, chatID)
	case "/alias":
		b.handleAlias(ctx, chatID, args)
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
	stopQueries, serviceNos, ok := parseAddArgs(args)
	if !ok {
		b.send(ctx, chatID, addUsage, false, nil)
		return
	}

	watchStops := make([]store.WatchStop, 0, len(stopQueries))
	seenStopCodes := make(map[string]bool)
	for _, stopQuery := range stopQueries {
		stops, err := b.lta.SearchStops(ctx, stopQuery, 6)
		if err != nil {
			b.fail(chatID, "search bus stops", err)
			return
		}
		if len(stops) == 0 {
			b.send(ctx, chatID, fmt.Sprintf("No bus stop matched %q. Try a bus stop code, name, or road.", stopQuery), false, nil)
			return
		}
		if len(stops) > 1 && !strings.EqualFold(stops[0].Description, stopQuery) && stops[0].BusStopCode != stopQuery {
			b.send(ctx, chatID, fmt.Sprintf("More than one stop matched %q.\n\n%s", stopQuery, formatStopMatches(stops)), false, nil)
			return
		}
		stop := stops[0]
		if seenStopCodes[stop.BusStopCode] {
			continue
		}
		seenStopCodes[stop.BusStopCode] = true
		watchStops = append(watchStops, store.WatchStop{
			Code:     stop.BusStopCode,
			Name:     stop.Description,
			RoadName: stop.RoadName,
		})
	}

	stopCodes := make([]string, 0, len(watchStops))
	for _, stop := range watchStops {
		stopCodes = append(stopCodes, stop.Code)
	}
	servicesByStop, err := b.lta.ServicesAtStops(ctx, stopCodes)
	if err != nil {
		b.fail(chatID, "validate bus services", err)
		return
	}
	watchStops, serviceNos, combinations := validWatchCombinations(watchStops, serviceNos, servicesByStop)
	if len(combinations) == 0 {
		b.send(ctx, chatID, "None of the requested bus services serve the selected bus stops.", false, nil)
		return
	}

	watch, err := b.store.Add(chatID, store.Watch{
		Stops:        watchStops,
		ServiceNos:   serviceNos,
		Combinations: combinations,
	})
	if errors.Is(err, store.ErrDuplicate) {
		b.send(ctx, chatID, "That combination of stops and services is already on your watchlist.", false, nil)
		return
	}
	if err != nil {
		b.fail(chatID, "save watch item", err)
		return
	}
	b.send(ctx, chatID, formatAddedWatch(watch), false, nil)
}

func (b *Bot) handleWatchlist(ctx context.Context, chatID int64) {
	watches := b.store.List(chatID)
	if len(watches) == 0 {
		b.send(ctx, chatID, emptyWatchlistHelp, false, nil)
		return
	}
	var text strings.Builder
	text.WriteString("Your watchlist:\n")
	for _, watch := range watches {
		fmt.Fprintf(&text, "\n%s", watchLabel(watch))
		writeWatchCombinations(&text, watch)
		if watch.Schedule != "" {
			fmt.Fprintf(&text, "\nDaily: %s", watch.Schedule)
		}
		text.WriteByte('\n')
	}
	text.WriteString("\nUse /alias <watch> <name> to add an alias. Commands accept an ID or alias.")
	b.send(ctx, chatID, text.String(), false, nil)
}

func (b *Bot) handleAlias(ctx context.Context, chatID int64, args string) {
	fields := strings.Fields(args)
	if len(fields) != 2 {
		b.send(ctx, chatID, "Usage: /alias <watch ID or alias> <new alias>\nExample: /alias 2 home", false, nil)
		return
	}
	if !validAlias(fields[1]) {
		b.send(ctx, chatID, "Aliases must be 1-32 characters, start with a letter, and contain only letters, numbers, hyphens, or underscores.", false, nil)
		return
	}
	watch, err := b.store.Resolve(chatID, fields[0])
	if errors.Is(err, store.ErrNotFound) {
		b.send(ctx, chatID, "No watch item has that ID or alias.", false, nil)
		return
	}
	if err != nil {
		b.fail(chatID, "load watch item", err)
		return
	}
	watch, err = b.store.SetAlias(chatID, watch.ID, fields[1])
	if errors.Is(err, store.ErrDuplicateAlias) {
		b.send(ctx, chatID, "Another watch already uses that alias.", false, nil)
		return
	}
	if err != nil {
		b.fail(chatID, "save watch alias", err)
		return
	}
	b.send(ctx, chatID, fmt.Sprintf("You can now refer to this watch as %q.", watch.Alias), false, nil)
}

func (b *Bot) handleDelete(ctx context.Context, chatID int64, args string) {
	if strings.TrimSpace(args) == "" {
		b.promptWatchSelection(ctx, chatID, "delete", "Choose a watch to delete:")
		return
	}
	if len(strings.Fields(args)) != 1 {
		b.send(ctx, chatID, "Usage: /delete <watch ID or alias>", false, nil)
		return
	}
	watch, err := b.store.Resolve(chatID, args)
	if errors.Is(err, store.ErrNotFound) {
		b.send(ctx, chatID, "No watch item has that ID or alias.", false, nil)
		return
	}
	if err != nil {
		b.fail(chatID, "load watch item", err)
		return
	}
	if err := b.store.Delete(chatID, watch.ID); errors.Is(err, store.ErrNotFound) {
		b.send(ctx, chatID, "No watch item has that ID or alias.", false, nil)
		return
	} else if err != nil {
		b.fail(chatID, "delete watch item", err)
		return
	}
	b.dismiss(chatID, watch.ID)
	b.send(ctx, chatID, fmt.Sprintf("Deleted %s.", watchLabel(watch)), false, nil)
}

func (b *Bot) handleNotify(ctx context.Context, chatID int64, args string) {
	if strings.TrimSpace(args) == "" {
		b.promptWatchSelection(ctx, chatID, "notify", "Choose a watch for ETA notifications:")
		return
	}
	if len(strings.Fields(args)) != 1 {
		b.send(ctx, chatID, "Usage: /notify <watch ID or alias>", false, nil)
		return
	}
	watch, err := b.store.Resolve(chatID, args)
	if errors.Is(err, store.ErrNotFound) {
		b.send(ctx, chatID, "No watch item has that ID or alias.", false, nil)
		return
	}
	if err != nil {
		b.fail(chatID, "load watch item", err)
		return
	}
	b.sendETA(ctx, chatID, watch, false)
}

func (b *Bot) handleSchedule(ctx context.Context, chatID int64, args string) {
	fields := strings.Fields(args)
	if len(fields) != 2 {
		b.send(ctx, chatID, "Usage: /schedule <watch ID or alias> <HH:MM>\nExample: /schedule home 07:30", false, nil)
		return
	}
	watch, err := b.store.Resolve(chatID, fields[0])
	if errors.Is(err, store.ErrNotFound) {
		b.send(ctx, chatID, "No watch item has that ID or alias.", false, nil)
		return
	}
	if err != nil {
		b.fail(chatID, "load watch item", err)
		return
	}
	parsed, err := time.Parse("15:04", fields[1])
	if err != nil {
		b.send(ctx, chatID, "Time must use 24-hour HH:MM format, for example 07:30.", false, nil)
		return
	}
	schedule := parsed.Format("15:04")
	watch, err = b.store.SetSchedule(chatID, watch.ID, schedule)
	if errors.Is(err, store.ErrNotFound) {
		b.send(ctx, chatID, "No watch item has that ID or alias.", false, nil)
		return
	}
	if err != nil {
		b.fail(chatID, "save schedule", err)
		return
	}
	b.send(ctx, chatID, fmt.Sprintf("%s will send a daily ETA prompt at %s (%s).", watchLabel(watch), schedule, b.location.String()), false, nil)
}

func (b *Bot) handleUnschedule(ctx context.Context, chatID int64, args string) {
	if strings.TrimSpace(args) == "" {
		b.promptWatchSelection(ctx, chatID, "unschedule", "Choose a watch to unschedule:")
		return
	}
	if len(strings.Fields(args)) != 1 {
		b.send(ctx, chatID, "Usage: /unschedule <watch ID or alias>", false, nil)
		return
	}
	watch, err := b.store.Resolve(chatID, args)
	if errors.Is(err, store.ErrNotFound) {
		b.send(ctx, chatID, "No watch item has that ID or alias.", false, nil)
		return
	}
	if err != nil {
		b.fail(chatID, "load watch item", err)
		return
	}
	watch, err = b.store.SetSchedule(chatID, watch.ID, "")
	if errors.Is(err, store.ErrNotFound) {
		b.send(ctx, chatID, "No watch item has that ID or alias.", false, nil)
		return
	}
	if err != nil {
		b.fail(chatID, "remove schedule", err)
		return
	}
	b.send(ctx, chatID, fmt.Sprintf("Removed the daily schedule from %s.", watchLabel(watch)), false, nil)
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
	watch, err := b.store.Get(chatID, id)
	if err != nil {
		b.answer(ctx, callback.ID, "That watch item no longer exists.")
		return
	}
	switch parts[0] {
	case "delete":
		if err := b.store.Delete(chatID, id); err != nil {
			b.fail(chatID, "delete watch item", err)
			b.answer(ctx, callback.ID, "Could not delete that watch.")
			return
		}
		b.dismiss(chatID, id)
		b.editKeyboard(ctx, chatID, callback.Message.MessageID, nil)
		b.send(ctx, chatID, fmt.Sprintf("Deleted %s.", watchLabel(watch)), false, nil)
		b.answer(ctx, callback.ID, "Watch deleted.")
	case "notify":
		b.editKeyboard(ctx, chatID, callback.Message.MessageID, nil)
		b.answer(ctx, callback.ID, "Fetching ETAs.")
		b.sendETA(ctx, chatID, watch, false)
	case "unschedule":
		if _, err := b.store.SetSchedule(chatID, id, ""); err != nil {
			b.fail(chatID, "remove schedule", err)
			b.answer(ctx, callback.ID, "Could not remove that schedule.")
			return
		}
		b.editKeyboard(ctx, chatID, callback.Message.MessageID, nil)
		b.send(ctx, chatID, fmt.Sprintf("Removed the daily schedule from %s.", watchLabel(watch)), false, nil)
		b.answer(ctx, callback.ID, "Schedule removed.")
	case "keep", "continue":
		b.activate(chatID, id, time.Now())
		b.editKeyboard(ctx, chatID, callback.Message.MessageID, notificationKeyboard(id, true))
		b.answer(ctx, callback.ID, "ETA updates enabled for 15 minutes.")
	case "dismiss":
		b.dismiss(chatID, id)
		b.editKeyboard(ctx, chatID, callback.Message.MessageID, notificationKeyboard(id, false))
		b.answer(ctx, callback.ID, "ETA updates dismissed.")
	default:
		b.answer(ctx, callback.ID, "Invalid action.")
	}
}

func (b *Bot) promptWatchSelection(ctx context.Context, chatID int64, action, prompt string) {
	watches := b.store.List(chatID)
	if len(watches) == 0 {
		b.send(ctx, chatID, emptyWatchlistHelp, false, nil)
		return
	}
	b.send(ctx, chatID, prompt, false, watchSelectionKeyboard(action, watches))
}

func (b *Bot) runScheduler(ctx context.Context) {
	jobs := make(chan notificationJob, schedulerQueueSize)
	var workers sync.WaitGroup
	for range schedulerWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				b.sendETA(ctx, job.chatID, job.watch, job.active)
			}
		}()
	}
	defer func() {
		close(jobs)
		workers.Wait()
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			localNow := now.In(b.location)
			for {
				dueSchedules, err := b.store.ClaimDue(localNow)
				if err != nil {
					b.log.Error("claim daily schedules", "error", err)
					break
				}
				for _, due := range dueSchedules {
					if !enqueueNotification(ctx, jobs, notificationJob{
						chatID: due.ChatID,
						watch:  due.Watch,
					}) {
						return
					}
				}
				if len(dueSchedules) == 0 {
					break
				}
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
				if !enqueueNotification(ctx, jobs, notificationJob{
					chatID: due.chatID,
					watch:  watch,
					active: true,
				}) {
					return
				}
			}
		}
	}
}

func enqueueNotification(ctx context.Context, jobs chan<- notificationJob, job notificationJob) bool {
	select {
	case jobs <- job:
		return true
	case <-ctx.Done():
		return false
	}
}

func (b *Bot) sendETA(ctx context.Context, chatID int64, watch store.Watch, active bool) {
	arrivalsByStop := make(map[string][]lta.ServiceArrival, len(watch.Stops))
	for _, stop := range watch.Stops {
		arrivals, err := b.lta.Arrivals(ctx, stop.Code, "")
		if err != nil {
			b.log.Error("fetch bus arrivals", "chat_id", chatID, "watch_id", watch.ID, "stop_code", stop.Code, "error", err)
			continue
		}
		arrivalsByStop[stop.Code] = arrivals
	}
	now := time.Now()
	text, urgent := formatETA(watch, arrivalsByStop, now)
	b.send(ctx, chatID, text, !urgent, notificationKeyboard(watch.ID, active))
}

func (b *Bot) activate(chatID int64, watchID int, now time.Time) {
	b.sessionsMu.Lock()
	defer b.sessionsMu.Unlock()
	key := sessionKey{chatID: chatID, watchID: watchID}
	active, exists := b.sessions[key]
	if !exists || now.After(active.expiresAt) || active.nextAt.After(active.expiresAt) {
		active.nextAt = now.Add(time.Minute)
	}
	active.expiresAt = now.Add(sessionDuration)
	b.sessions[key] = active
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
		if active.nextAt.After(active.expiresAt) || now.After(active.expiresAt.Add(sessionExpiryGrace)) {
			delete(b.sessions, key)
			continue
		}
		if now.Before(active.nextAt) {
			continue
		}
		due = append(due, key)
		active.nextAt = active.nextAt.Add(time.Minute)
		b.sessions[key] = active
	}
	return due
}

func (b *Bot) registerCommands(ctx context.Context) error {
	return b.telegram.SetCommands(ctx, []telegram.BotCommand{
		{Command: "add", Description: "Add a stop and service"},
		{Command: "watchlist", Description: "Show your watchlist"},
		{Command: "alias", Description: "Add an alias to a watch"},
		{Command: "delete", Description: "Delete a watch"},
		{Command: "notify", Description: "Send an ETA prompt"},
		{Command: "schedule", Description: "Schedule a daily ETA prompt"},
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

func (b *Bot) editKeyboard(ctx context.Context, chatID, messageID int64, keyboard *telegram.InlineKeyboardMarkup) {
	if err := b.telegram.EditMessageReplyMarkup(ctx, chatID, messageID, keyboard); err != nil {
		b.log.Error("edit Telegram keyboard", "chat_id", chatID, "message_id", messageID, "error", err)
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

func parseAddArgs(args string) ([]string, []string, bool) {
	parts := strings.SplitN(args, ";", 2)
	if len(parts) != 2 {
		return nil, nil, false
	}
	stops := splitUnique(parts[0], false)
	services := splitUnique(parts[1], true)
	if len(stops) == 0 || len(services) == 0 {
		return nil, nil, false
	}
	for _, service := range services {
		if !validServiceNo(service) {
			return nil, nil, false
		}
	}
	return stops, services, true
}

func splitUnique(value string, upper bool) []string {
	var result []string
	seen := make(map[string]bool)
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if upper {
			part = strings.ToUpper(part)
		}
		key := strings.ToUpper(part)
		if part == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, part)
	}
	return result
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

func validAlias(alias string) bool {
	if len(alias) < 1 || len(alias) > 32 {
		return false
	}
	for i, r := range alias {
		if i == 0 && !isASCIILetter(r) {
			return false
		}
		if !isASCIILetter(r) && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func formatStopMatches(stops []lta.BusStop) string {
	var text strings.Builder
	text.WriteString("Matching bus stops:\n")
	for _, stop := range stops {
		fmt.Fprintf(&text, "\n%s - %s, %s", stop.BusStopCode, stop.Description, stop.RoadName)
	}
	text.WriteString("\n\nAdd with /add <code[, code...]> ; <service[, service...]>.")
	return text.String()
}

func validWatchCombinations(stops []store.WatchStop, serviceNos []string, servicesByStop map[string][]string) ([]store.WatchStop, []string, []store.WatchCombination) {
	var combinations []store.WatchCombination
	usedStops := make(map[string]bool)
	usedServices := make(map[string]bool)
	for _, stop := range stops {
		served := make(map[string]bool, len(servicesByStop[stop.Code]))
		for _, serviceNo := range servicesByStop[stop.Code] {
			served[strings.ToUpper(serviceNo)] = true
		}
		for _, serviceNo := range serviceNos {
			if !served[strings.ToUpper(serviceNo)] {
				continue
			}
			combinations = append(combinations, store.WatchCombination{
				StopCode:  stop.Code,
				ServiceNo: serviceNo,
			})
			usedStops[stop.Code] = true
			usedServices[strings.ToUpper(serviceNo)] = true
		}
	}

	validStops := make([]store.WatchStop, 0, len(usedStops))
	for _, stop := range stops {
		if usedStops[stop.Code] {
			validStops = append(validStops, stop)
		}
	}
	validServices := make([]string, 0, len(usedServices))
	for _, serviceNo := range serviceNos {
		if usedServices[strings.ToUpper(serviceNo)] {
			validServices = append(validServices, serviceNo)
		}
	}
	return validStops, validServices, combinations
}

type etaResult struct {
	stop        store.WatchStop
	serviceNo   string
	arrivals    []lta.Arrival
	firstETA    time.Time
	hasFirstETA bool
}

func formatETA(watch store.Watch, arrivalsByStop map[string][]lta.ServiceArrival, now time.Time) (string, bool) {
	stopsByCode := make(map[string]store.WatchStop, len(watch.Stops))
	for _, stop := range watch.Stops {
		stopsByCode[stop.Code] = stop
	}
	combinations := watchCombinations(watch)
	results := make([]etaResult, 0, len(combinations))
	for _, combination := range combinations {
		stop, ok := stopsByCode[combination.StopCode]
		if !ok {
			continue
		}
		services := arrivalsByStop[stop.Code]
		result := etaResult{stop: stop, serviceNo: combination.ServiceNo}
		for i := range services {
			if !strings.EqualFold(services[i].ServiceNo, combination.ServiceNo) {
				continue
			}
			result.arrivals = []lta.Arrival{services[i].NextBus, services[i].NextBus2, services[i].NextBus3}
			for _, arrival := range result.arrivals {
				parsed, err := time.Parse(time.RFC3339, arrival.EstimatedArrival)
				if err == nil {
					result.firstETA = parsed
					result.hasFirstETA = true
					break
				}
			}
			break
		}
		results = append(results, result)
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].hasFirstETA != results[j].hasFirstETA {
			return results[i].hasFirstETA
		}
		if !results[i].hasFirstETA {
			return false
		}
		return results[i].firstETA.Before(results[j].firstETA)
	})

	var text strings.Builder
	fmt.Fprintf(&text, "%s ETAs:", watchLabel(watch))
	urgent := false
	for _, result := range results {
		var labels []string
		for _, bus := range result.arrivals {
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
		fmt.Fprintf(&text, "\n\nBus %s at %s (%s)\n", result.serviceNo, result.stop.Name, result.stop.Code)
		if len(labels) == 0 {
			text.WriteString("No ETA available.")
		} else {
			text.WriteString("ETA: " + strings.Join(labels, ", "))
		}
	}
	return text.String(), urgent
}

func formatAddedWatch(watch store.Watch) string {
	var text strings.Builder
	fmt.Fprintf(&text, "Added watch #%d:", watch.ID)
	writeWatchCombinations(&text, watch)
	return text.String()
}

func watchLabel(watch store.Watch) string {
	if watch.Alias == "" {
		return fmt.Sprintf("Watch #%d", watch.ID)
	}
	return fmt.Sprintf("Watch %s", watch.Alias)
}

func writeWatchCombinations(text *strings.Builder, watch store.Watch) {
	stopsByCode := make(map[string]store.WatchStop, len(watch.Stops))
	for _, stop := range watch.Stops {
		stopsByCode[stop.Code] = stop
	}
	for _, combination := range watchCombinations(watch) {
		stop := stopsByCode[combination.StopCode]
		fmt.Fprintf(text, "\nBus %s at %s (%s)", combination.ServiceNo, stop.Name, stop.Code)
	}
}

func watchCombinations(watch store.Watch) []store.WatchCombination {
	if len(watch.Combinations) > 0 {
		return watch.Combinations
	}
	var combinations []store.WatchCombination
	for _, stop := range watch.Stops {
		for _, serviceNo := range watch.ServiceNos {
			combinations = append(combinations, store.WatchCombination{
				StopCode:  stop.Code,
				ServiceNo: serviceNo,
			})
		}
	}
	return combinations
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

func notificationKeyboard(watchID int, active bool) *telegram.InlineKeyboardMarkup {
	buttons := []telegram.InlineKeyboardButton{{
		Text:         "Keep notifying (15 mins)",
		CallbackData: fmt.Sprintf("keep:%d", watchID),
	}}
	if active {
		buttons = append(buttons, telegram.InlineKeyboardButton{
			Text:         "Dismiss",
			CallbackData: fmt.Sprintf("dismiss:%d", watchID),
		})
	}
	return &telegram.InlineKeyboardMarkup{
		InlineKeyboard: [][]telegram.InlineKeyboardButton{buttons},
	}
}

func watchSelectionKeyboard(action string, watches []store.Watch) *telegram.InlineKeyboardMarkup {
	rows := make([][]telegram.InlineKeyboardButton, 0, len(watches))
	for _, watch := range watches {
		rows = append(rows, []telegram.InlineKeyboardButton{{
			Text:         watchSelectionLabel(watch),
			CallbackData: fmt.Sprintf("%s:%d", action, watch.ID),
		}})
	}
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func watchSelectionLabel(watch store.Watch) string {
	combinations := watchCombinations(watch)
	if len(combinations) == 0 {
		return watchLabel(watch)
	}
	stopsByCode := make(map[string]store.WatchStop, len(watch.Stops))
	for _, stop := range watch.Stops {
		stopsByCode[stop.Code] = stop
	}
	first := combinations[0]
	stop := stopsByCode[first.StopCode]
	stopName := stop.Name
	if stopName == "" {
		stopName = first.StopCode
	}
	label := watch.Alias
	if label == "" {
		label = fmt.Sprintf("#%d", watch.ID)
	}
	label += fmt.Sprintf(" Bus %s at %s", first.ServiceNo, stopName)
	if len(combinations) > 1 {
		label += fmt.Sprintf(" +%d", len(combinations)-1)
	}
	return label
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

/add <stop[, stop...]> ; <service[, service...]> - add a watch
/watchlist - list watches, IDs, and aliases
/alias <watch> <name> - add or change a watch alias
/delete <watch> - delete a watch
/notify <watch> - send an ETA prompt
/schedule <watch> <HH:MM> - schedule a daily ETA prompt
/unschedule <watch> - remove a daily schedule`
