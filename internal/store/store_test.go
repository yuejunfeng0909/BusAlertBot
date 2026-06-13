package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreIDsPersistAndDoNotReuseDeletedIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	data, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	first, err := data.Add(42, Watch{StopCode: "01019", StopName: "Hotel Grand Pacific", ServiceNo: "7"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != 1 {
		t.Fatalf("first ID = %d, want 1", first.ID)
	}
	if err := data.Delete(42, first.ID); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := reopened.Add(42, Watch{StopCode: "02049", StopName: "Raffles Hotel", ServiceNo: "36"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != 2 {
		t.Fatalf("second ID = %d, want 2", second.ID)
	}
}

func TestStoreRejectsDuplicateWatch(t *testing.T) {
	data, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	watch := Watch{StopCode: "02049", StopName: "Raffles Hotel", ServiceNo: "36"}
	if _, err := data.Add(1, watch); err != nil {
		t.Fatal(err)
	}
	if _, err := data.Add(1, watch); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate error = %v, want ErrDuplicate", err)
	}
}

func TestClaimDueOnlyOncePerMinute(t *testing.T) {
	data, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	watch, err := data.Add(99, Watch{StopCode: "02049", StopName: "Raffles Hotel", ServiceNo: "36"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := data.SetSchedule(99, watch.ID, "07:30"); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, time.June, 13, 7, 30, 5, 0, time.FixedZone("SGT", 8*60*60))
	due, err := data.ClaimDue(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("first claim returned %d watches, want 1", len(due))
	}
	due, err = data.ClaimDue(now.Add(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("second claim returned %d watches, want 0", len(due))
	}
}
