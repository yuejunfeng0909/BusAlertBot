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
	watch := store.Watch{ID: 3, StopCode: "02049", StopName: "Raffles Hotel", ServiceNo: "36"}
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

	text, urgent := formatETA(watch, services, now)
	if !urgent {
		t.Fatal("urgent = false, want true")
	}
	if !strings.Contains(text, "1 min (seats), 5 min (standing)") {
		t.Fatalf("text = %q", text)
	}
}

func TestFormatETAAtTwoMinutesIsSilent(t *testing.T) {
	now := time.Date(2026, time.June, 13, 7, 30, 0, 0, time.UTC)
	watch := store.Watch{ID: 1, StopCode: "02049", StopName: "Raffles Hotel", ServiceNo: "36"}
	services := []lta.ServiceArrival{{
		ServiceNo: "36",
		NextBus: lta.Arrival{
			EstimatedArrival: now.Add(2 * time.Minute).Format(time.RFC3339),
		},
	}}

	_, urgent := formatETA(watch, services, now)
	if urgent {
		t.Fatal("urgent = true at exactly two minutes, want false")
	}
}

func TestParseAddArgs(t *testing.T) {
	stop, service, ok := parseAddArgs(" Raffles Hotel | 36a ")
	if !ok || stop != "Raffles Hotel" || service != "36A" {
		t.Fatalf("got stop=%q service=%q ok=%v", stop, service, ok)
	}
}
