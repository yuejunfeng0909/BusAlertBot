package bot

import (
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
