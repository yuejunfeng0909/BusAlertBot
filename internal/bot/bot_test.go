package bot

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"busalertbot/internal/lta"
	"busalertbot/internal/store"
	"busalertbot/internal/telegram"
)

type sentMessage struct {
	text     string
	keyboard *telegram.InlineKeyboardMarkup
}

type fakeTelegramClient struct {
	messages []sentMessage
	answers  []string
	edits    []*telegram.InlineKeyboardMarkup
}

func (f *fakeTelegramClient) GetUpdates(context.Context, int64, time.Duration) ([]telegram.Update, error) {
	return nil, nil
}

func (f *fakeTelegramClient) SendMessage(_ context.Context, _ int64, text string, _ bool, keyboard *telegram.InlineKeyboardMarkup) error {
	f.messages = append(f.messages, sentMessage{text: text, keyboard: keyboard})
	return nil
}

func (f *fakeTelegramClient) AnswerCallback(_ context.Context, _, text string) error {
	f.answers = append(f.answers, text)
	return nil
}

func (f *fakeTelegramClient) EditMessageReplyMarkup(_ context.Context, _, _ int64, keyboard *telegram.InlineKeyboardMarkup) error {
	f.edits = append(f.edits, keyboard)
	return nil
}

func (f *fakeTelegramClient) SetCommands(context.Context, []telegram.BotCommand) error {
	return nil
}

type fakeLTAClient struct{}

func (fakeLTAClient) SearchStops(context.Context, string, int) ([]lta.BusStop, error) {
	return nil, nil
}

func (fakeLTAClient) Arrivals(context.Context, string, string) ([]lta.ServiceArrival, error) {
	return nil, nil
}

func (fakeLTAClient) ServicesAtStops(context.Context, []string) (map[string][]string, error) {
	return nil, nil
}

func newTestBot(t *testing.T) (*Bot, *store.Store, *fakeTelegramClient) {
	t.Helper()
	data, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeTelegramClient{}
	return New(slog.Default(), data, fakeLTAClient{}, client, time.UTC, time.Second), data, client
}

func addTestWatch(t *testing.T, data *store.Store, chatID int64) store.Watch {
	t.Helper()
	watch, err := data.Add(chatID, store.Watch{
		Stops:        []store.WatchStop{{Code: "02049", Name: "Raffles Hotel"}},
		ServiceNos:   []string{"36"},
		Combinations: []store.WatchCombination{{StopCode: "02049", ServiceNo: "36"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return watch
}

func TestArgumentlessWatchCommandsShowWatchChoices(t *testing.T) {
	b, data, client := newTestBot(t)
	const chatID int64 = 42
	watch := addTestWatch(t, data, chatID)
	watch, err := data.SetAlias(chatID, watch.ID, "home")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		action string
		call   func()
	}{
		{name: "delete", action: "delete", call: func() { b.handleDelete(context.Background(), chatID, "") }},
		{name: "notify", action: "notify", call: func() { b.handleNotify(context.Background(), chatID, "  ") }},
		{name: "unschedule", action: "unschedule", call: func() { b.handleUnschedule(context.Background(), chatID, "") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client.messages = nil
			test.call()
			if len(client.messages) != 1 {
				t.Fatalf("sent %d messages, want 1", len(client.messages))
			}
			keyboard := client.messages[0].keyboard
			if keyboard == nil || len(keyboard.InlineKeyboard) != 1 || len(keyboard.InlineKeyboard[0]) != 1 {
				t.Fatalf("keyboard = %#v", keyboard)
			}
			button := keyboard.InlineKeyboard[0][0]
			if button.Text != "home Bus 36 at Raffles Hotel" {
				t.Fatalf("button text = %q", button.Text)
			}
			wantCallback := test.action + ":" + strconv.Itoa(watch.ID)
			if button.CallbackData != wantCallback {
				t.Fatalf("callback data = %q, want %q", button.CallbackData, wantCallback)
			}
		})
	}
}

func TestAliasConfirmationHidesWatchID(t *testing.T) {
	b, data, client := newTestBot(t)
	const chatID int64 = 42
	addTestWatch(t, data, chatID)

	b.handleAlias(context.Background(), chatID, "1 home")

	if len(client.messages) != 1 {
		t.Fatalf("sent %d messages, want 1", len(client.messages))
	}
	if got := client.messages[0].text; got != `You can now refer to this watch as "home".` {
		t.Fatalf("message = %q", got)
	}
}

func TestWatchSelectionCallbacksPerformActions(t *testing.T) {
	b, data, client := newTestBot(t)
	const chatID int64 = 42

	unscheduled := addTestWatch(t, data, chatID)
	if _, err := data.SetSchedule(chatID, unscheduled.ID, "07:30"); err != nil {
		t.Fatal(err)
	}
	b.handleCallback(context.Background(), &telegram.CallbackQuery{
		ID:   "unschedule-callback",
		Data: "unschedule:1",
		Message: &telegram.Message{
			MessageID: 10,
			Chat:      telegram.Chat{ID: chatID},
		},
	})
	updated, err := data.Get(chatID, unscheduled.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Schedule != "" {
		t.Fatalf("schedule = %q, want empty", updated.Schedule)
	}

	b.handleCallback(context.Background(), &telegram.CallbackQuery{
		ID:   "notify-callback",
		Data: "notify:1",
		Message: &telegram.Message{
			MessageID: 11,
			Chat:      telegram.Chat{ID: chatID},
		},
	})
	if len(client.messages) < 2 || !strings.Contains(client.messages[len(client.messages)-1].text, "Watch #1 ETAs:") {
		t.Fatalf("messages = %#v", client.messages)
	}

	b.handleCallback(context.Background(), &telegram.CallbackQuery{
		ID:   "delete-callback",
		Data: "delete:1",
		Message: &telegram.Message{
			MessageID: 12,
			Chat:      telegram.Chat{ID: chatID},
		},
	})
	if _, err := data.Get(chatID, unscheduled.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get deleted watch error = %v, want ErrNotFound", err)
	}
}

func TestFormatETAIsUrgentBelowTwoMinutes(t *testing.T) {
	now := time.Date(2026, time.June, 13, 7, 30, 0, 0, time.FixedZone("SGT", 8*60*60))
	watch := store.Watch{
		ID:         3,
		Stops:      []store.WatchStop{{Code: "02049", Name: "Raffles Hotel"}},
		ServiceNos: []string{"36"},
	}
	services := []lta.ServiceArrival{{
		ServiceNo: "36",
		NextBus: lta.Arrival{
			EstimatedArrival: now.Add(119 * time.Second).Format(time.RFC3339),
			Load:             "SEA",
		},
		NextBus2: lta.Arrival{
			EstimatedArrival: now.Add(5*time.Minute + 59*time.Second).Format(time.RFC3339),
			Load:             "SDA",
		},
	}}

	text, urgent := formatETA(watch, map[string][]lta.ServiceArrival{"02049": services}, now)
	if !urgent {
		t.Fatal("urgent = false, want true")
	}
	if !strings.Contains(text, "1 min (seats), 5 min (standing)") {
		t.Fatalf("text = %q", text)
	}
}

func TestFormatETAAtTwoMinutesIsSilent(t *testing.T) {
	now := time.Date(2026, time.June, 13, 7, 30, 0, 0, time.UTC)
	watch := store.Watch{
		ID:         1,
		Stops:      []store.WatchStop{{Code: "02049", Name: "Raffles Hotel"}},
		ServiceNos: []string{"36"},
	}
	services := []lta.ServiceArrival{{
		ServiceNo: "36",
		NextBus: lta.Arrival{
			EstimatedArrival: now.Add(2 * time.Minute).Format(time.RFC3339),
		},
	}}

	_, urgent := formatETA(watch, map[string][]lta.ServiceArrival{"02049": services}, now)
	if urgent {
		t.Fatal("urgent = true at exactly two minutes, want false")
	}
}

func TestParseAddArgs(t *testing.T) {
	stops, services, ok := parseAddArgs(" 02049, 04167, 02049 | 36a, 111 ")
	if !ok || strings.Join(stops, ",") != "02049,04167" || strings.Join(services, ",") != "36A,111" {
		t.Fatalf("got stops=%q services=%q ok=%v", stops, services, ok)
	}
}

func TestValidAlias(t *testing.T) {
	for _, alias := range []string{"home", "Work-36", "school_bus", "a1"} {
		if !validAlias(alias) {
			t.Errorf("validAlias(%q) = false, want true", alias)
		}
	}
	for _, alias := range []string{"", "1home", "two words", "home!", strings.Repeat("a", 33)} {
		if validAlias(alias) {
			t.Errorf("validAlias(%q) = true, want false", alias)
		}
	}
}

func TestWatchLabelIncludesAlias(t *testing.T) {
	if got := watchLabel(store.Watch{ID: 3, Alias: "home"}); got != "Watch home" {
		t.Fatalf("watchLabel() = %q", got)
	}
	if got := watchLabel(store.Watch{ID: 3}); got != "Watch #3" {
		t.Fatalf("watchLabel() = %q", got)
	}
}

func TestFormatETASortsStopServiceCombinationsByNextArrival(t *testing.T) {
	now := time.Date(2026, time.June, 13, 7, 30, 0, 0, time.UTC)
	watch := store.Watch{
		ID: 7,
		Stops: []store.WatchStop{
			{Code: "02049", Name: "Raffles Hotel"},
			{Code: "04167", Name: "Stamford Court"},
		},
		ServiceNos: []string{"36", "111"},
	}
	arrivals := map[string][]lta.ServiceArrival{
		"02049": {
			{ServiceNo: "36", NextBus: lta.Arrival{EstimatedArrival: now.Add(8 * time.Minute).Format(time.RFC3339)}},
			{ServiceNo: "111", NextBus: lta.Arrival{EstimatedArrival: now.Add(3 * time.Minute).Format(time.RFC3339)}},
		},
		"04167": {
			{ServiceNo: "36", NextBus: lta.Arrival{EstimatedArrival: now.Add(5 * time.Minute).Format(time.RFC3339)}},
		},
	}

	text, _ := formatETA(watch, arrivals, now)
	expectedOrder := []string{
		"Bus 111 at Raffles Hotel",
		"Bus 36 at Stamford Court",
		"Bus 36 at Raffles Hotel",
		"Bus 111 at Stamford Court",
	}
	last := -1
	for _, expected := range expectedOrder {
		index := strings.Index(text, expected)
		if index <= last {
			t.Fatalf("%q is out of order in:\n%s", expected, text)
		}
		last = index
	}
}

func TestValidWatchCombinationsKeepsOnlyServedPairs(t *testing.T) {
	stops := []store.WatchStop{
		{Code: "02049", Name: "Raffles Hotel"},
		{Code: "04167", Name: "Stamford Court"},
		{Code: "99999", Name: "Unused Stop"},
	}
	services := []string{"36", "111", "999"}
	servicesByStop := map[string][]string{
		"02049": {"36"},
		"04167": {"111"},
	}

	validStops, validServices, combinations := validWatchCombinations(stops, services, servicesByStop)
	if len(validStops) != 2 || validStops[0].Code != "02049" || validStops[1].Code != "04167" {
		t.Fatalf("valid stops = %#v", validStops)
	}
	if strings.Join(validServices, ",") != "36,111" {
		t.Fatalf("valid services = %#v", validServices)
	}
	if len(combinations) != 2 ||
		combinations[0] != (store.WatchCombination{StopCode: "02049", ServiceNo: "36"}) ||
		combinations[1] != (store.WatchCombination{StopCode: "04167", ServiceNo: "111"}) {
		t.Fatalf("combinations = %#v", combinations)
	}
}

func TestNotificationKeyboardShowsDismissOnlyForActiveSession(t *testing.T) {
	prompt := notificationKeyboard(4, false).InlineKeyboard[0]
	if len(prompt) != 1 || prompt[0].Text != "Keep notifying (15 mins)" || prompt[0].CallbackData != "keep:4" {
		t.Fatalf("prompt buttons = %#v", prompt)
	}

	active := notificationKeyboard(4, true).InlineKeyboard[0]
	if len(active) != 2 || active[0].Text != "Keep notifying (15 mins)" || active[1].Text != "Dismiss" {
		t.Fatalf("active buttons = %#v", active)
	}
}

func TestSessionSendsEveryMinuteForFifteenMinutes(t *testing.T) {
	b := &Bot{log: slog.Default(), sessions: make(map[sessionKey]session)}
	start := time.Date(2026, time.June, 13, 7, 30, 0, 0, time.UTC)
	b.activate(42, 3, start)

	for minute := 1; minute <= 15; minute++ {
		due := b.dueSessions(start.Add(time.Duration(minute) * time.Minute))
		if len(due) != 1 {
			t.Fatalf("minute %d returned %d due sessions, want 1", minute, len(due))
		}
	}
	if due := b.dueSessions(start.Add(16 * time.Minute)); len(due) != 0 {
		t.Fatalf("minute 16 returned %d due sessions, want 0", len(due))
	}
}

func TestSessionDoesNotReplayUpdatesAfterExpiry(t *testing.T) {
	b := &Bot{log: slog.Default(), sessions: make(map[sessionKey]session)}
	start := time.Date(2026, time.June, 13, 7, 30, 0, 0, time.UTC)
	b.activate(42, 3, start)

	if due := b.dueSessions(start.Add(20 * time.Minute)); len(due) != 0 {
		t.Fatalf("expired session returned %d due sessions, want 0", len(due))
	}
}

func TestKeepNotifyingExtendsFromClickWithoutResettingCadence(t *testing.T) {
	b := &Bot{log: slog.Default(), sessions: make(map[sessionKey]session)}
	start := time.Date(2026, time.June, 13, 7, 30, 0, 0, time.UTC)
	b.activate(42, 3, start)

	for minute := 1; minute <= 10; minute++ {
		b.dueSessions(start.Add(time.Duration(minute) * time.Minute))
	}
	b.activate(42, 3, start.Add(10*time.Minute+30*time.Second))

	key := sessionKey{chatID: 42, watchID: 3}
	active := b.sessions[key]
	if !active.nextAt.Equal(start.Add(11 * time.Minute)) {
		t.Fatalf("next update = %s, want %s", active.nextAt, start.Add(11*time.Minute))
	}
	wantExpiry := start.Add(25*time.Minute + 30*time.Second)
	if !active.expiresAt.Equal(wantExpiry) {
		t.Fatalf("expiry = %s, want %s", active.expiresAt, wantExpiry)
	}
}
