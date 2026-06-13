package bot

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"busalertbot/internal/lta"
	"busalertbot/internal/store"
)

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
