package store

import (
	"errors"
	"os"
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

func TestStoreRejectsReorderedMultiWatchAsDuplicate(t *testing.T) {
	data, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	first := Watch{
		Stops:      []WatchStop{{Code: "02049"}, {Code: "04167"}},
		ServiceNos: []string{"36", "111"},
	}
	if _, err := data.Add(1, first); err != nil {
		t.Fatal(err)
	}
	reordered := Watch{
		Stops:      []WatchStop{{Code: "04167"}, {Code: "02049"}},
		ServiceNos: []string{"111", "36"},
	}
	if _, err := data.Add(1, reordered); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate error = %v, want ErrDuplicate", err)
	}
}

func TestStoreAliasesResolveCaseInsensitivelyAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	data, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	watch, err := data.Add(42, Watch{StopCode: "02049", StopName: "Raffles Hotel", ServiceNo: "36"})
	if err != nil {
		t.Fatal(err)
	}
	watch, err = data.SetAlias(42, watch.ID, "Home-36")
	if err != nil {
		t.Fatal(err)
	}
	if watch.Alias != "home-36" {
		t.Fatalf("alias = %q, want home-36", watch.Alias)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := reopened.Resolve(42, "HOME-36")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != watch.ID {
		t.Fatalf("resolved ID = %d, want %d", resolved.ID, watch.ID)
	}
	resolved, err = reopened.Resolve(42, "1")
	if err != nil || resolved.ID != watch.ID {
		t.Fatalf("numeric resolve = %#v, %v", resolved, err)
	}
}

func TestStoreRejectsDuplicateAliasWithinChat(t *testing.T) {
	data, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := data.Add(42, Watch{StopCode: "02049", ServiceNo: "36"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := data.Add(42, Watch{StopCode: "04167", ServiceNo: "111"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := data.SetAlias(42, first.ID, "home"); err != nil {
		t.Fatal(err)
	}
	if _, err := data.SetAlias(42, second.ID, "HOME"); !errors.Is(err, ErrDuplicateAlias) {
		t.Fatalf("duplicate alias error = %v, want ErrDuplicateAlias", err)
	}
	otherChat, err := data.Add(99, Watch{StopCode: "04167", ServiceNo: "111"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := data.SetAlias(99, otherChat.ID, "home"); err != nil {
		t.Fatalf("reuse alias in another chat: %v", err)
	}
	if _, err := data.SetAlias(42, 999, "home"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing watch error = %v, want ErrNotFound", err)
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

func TestOpenMigratesLegacySingleStopWatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"users":{"42":{"next_id":2,"watches":[{"id":1,"stop_code":"02049","stop_name":"Raffles Hotel","road_name":"Bras Basah Rd","service_no":"36"}]}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	data, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	watch, err := data.Get(42, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(watch.Stops) != 1 || watch.Stops[0].Code != "02049" {
		t.Fatalf("stops = %#v", watch.Stops)
	}
	if len(watch.ServiceNos) != 1 || watch.ServiceNos[0] != "36" {
		t.Fatalf("service numbers = %#v", watch.ServiceNos)
	}
	if len(watch.Combinations) != 1 || watch.Combinations[0].StopCode != "02049" || watch.Combinations[0].ServiceNo != "36" {
		t.Fatalf("combinations = %#v", watch.Combinations)
	}
}
