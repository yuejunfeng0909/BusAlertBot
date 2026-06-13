package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound       = errors.New("watch item not found")
	ErrDuplicate      = errors.New("watch item already exists")
	ErrDuplicateAlias = errors.New("watch alias already exists")
)

const dueBatchSize = 500

type Watch struct {
	ID            int                `json:"id"`
	Alias         string             `json:"alias,omitempty"`
	Stops         []WatchStop        `json:"stops,omitempty"`
	ServiceNos    []string           `json:"service_nos,omitempty"`
	Combinations  []WatchCombination `json:"combinations,omitempty"`
	StopCode      string             `json:"stop_code,omitempty"`
	StopName      string             `json:"stop_name,omitempty"`
	RoadName      string             `json:"road_name,omitempty"`
	ServiceNo     string             `json:"service_no,omitempty"`
	Schedule      string             `json:"schedule,omitempty"`
	LastTriggered string             `json:"last_triggered,omitempty"`
}

type WatchCombination struct {
	StopCode  string `json:"stop_code"`
	ServiceNo string `json:"service_no"`
}

type WatchStop struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	RoadName string `json:"road_name,omitempty"`
}

type User struct {
	NextID  int     `json:"next_id"`
	Watches []Watch `json:"watches"`
}

type legacyState struct {
	Users map[string]*User `json:"users"`
}

type Store struct {
	db *sql.DB
}

type DueWatch struct {
	ChatID int64
	Watch  Watch
}

func Open(path string) (*Store, error) {
	dbPath, legacyPath := persistencePaths(path)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}

	dsn := (&url.URL{Scheme: "file", Path: dbPath}).String()
	dsn += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite state: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	s := &Store{db: db}
	if err := s.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("secure sqlite state: %w", err)
	}
	if legacyPath != "" {
		if err := s.migrateLegacyJSON(legacyPath); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Add(chatID int64, watch Watch) (Watch, error) {
	normalizeWatch(&watch)

	tx, err := s.db.Begin()
	if err != nil {
		return Watch{}, fmt.Errorf("begin add watch: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO users (chat_id, next_id) VALUES (?, 1)
		ON CONFLICT(chat_id) DO NOTHING
	`, chatID); err != nil {
		return Watch{}, fmt.Errorf("ensure user: %w", err)
	}
	if err := tx.QueryRow(`
		UPDATE users SET next_id = next_id + 1
		WHERE chat_id = ?
		RETURNING next_id - 1
	`, chatID).Scan(&watch.ID); err != nil {
		return Watch{}, fmt.Errorf("allocate watch ID: %w", err)
	}
	payload, err := encodeWatch(watch)
	if err != nil {
		return Watch{}, err
	}
	_, err = tx.Exec(`
		INSERT INTO watches (
			chat_id, watch_id, alias, signature, payload, schedule, schedule_minute, last_triggered
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, chatID, watch.ID, watch.Alias, watchSignature(watch), payload, watch.Schedule, scheduleMinute(watch.Schedule), watch.LastTriggered)
	if err != nil {
		return Watch{}, classifyConstraint(err)
	}
	if err := tx.Commit(); err != nil {
		return Watch{}, fmt.Errorf("commit add watch: %w", err)
	}
	return watch, nil
}

func (s *Store) List(chatID int64) []Watch {
	rows, err := s.db.Query(`
		SELECT watch_id, alias, payload, schedule, last_triggered
		FROM watches WHERE chat_id = ? ORDER BY watch_id
	`, chatID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var watches []Watch
	for rows.Next() {
		watch, err := scanWatch(rows)
		if err != nil {
			return nil
		}
		watches = append(watches, watch)
	}
	return watches
}

func (s *Store) Get(chatID int64, id int) (Watch, error) {
	return queryWatch(s.db.QueryRow(`
		SELECT watch_id, alias, payload, schedule, last_triggered
		FROM watches WHERE chat_id = ? AND watch_id = ?
	`, chatID, id))
}

func (s *Store) Resolve(chatID int64, reference string) (Watch, error) {
	reference = strings.TrimSpace(reference)
	if id, err := strconv.Atoi(reference); err == nil {
		return s.Get(chatID, id)
	}
	return queryWatch(s.db.QueryRow(`
		SELECT watch_id, alias, payload, schedule, last_triggered
		FROM watches WHERE chat_id = ? AND alias = ? COLLATE NOCASE
	`, chatID, reference))
}

func (s *Store) SetAlias(chatID int64, id int, alias string) (Watch, error) {
	alias = strings.ToLower(strings.TrimSpace(alias))
	result, err := s.db.Exec(`
		UPDATE watches SET alias = ? WHERE chat_id = ? AND watch_id = ?
	`, alias, chatID, id)
	if err != nil {
		return Watch{}, classifyConstraint(err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return Watch{}, fmt.Errorf("check alias update: %w", err)
	} else if affected == 0 {
		return Watch{}, ErrNotFound
	}
	return s.Get(chatID, id)
}

func (s *Store) Delete(chatID int64, id int) error {
	result, err := s.db.Exec(`DELETE FROM watches WHERE chat_id = ? AND watch_id = ?`, chatID, id)
	if err != nil {
		return fmt.Errorf("delete watch: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check deleted watch: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetSchedule(chatID int64, id int, schedule string) (Watch, error) {
	result, err := s.db.Exec(`
		UPDATE watches
		SET schedule = ?, schedule_minute = ?, last_triggered = ''
		WHERE chat_id = ? AND watch_id = ?
	`, schedule, scheduleMinute(schedule), chatID, id)
	if err != nil {
		return Watch{}, fmt.Errorf("set watch schedule: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Watch{}, fmt.Errorf("check schedule update: %w", err)
	}
	if affected == 0 {
		return Watch{}, ErrNotFound
	}
	return s.Get(chatID, id)
}

func (s *Store) ClaimDue(now time.Time) ([]DueWatch, error) {
	minute := now.Format("2006-01-02 15:04")
	rows, err := s.db.Query(`
		WITH due AS (
			SELECT rowid
			FROM watches
			WHERE schedule_minute = ?
			  AND last_triggered <> ?
			LIMIT ?
		)
		UPDATE watches
		SET last_triggered = ?
		WHERE rowid IN (SELECT rowid FROM due)
		  AND last_triggered <> ?
		RETURNING chat_id, watch_id, alias, payload, schedule, last_triggered
	`, now.Hour()*60+now.Minute(), minute, dueBatchSize, minute, minute)
	if err != nil {
		return nil, fmt.Errorf("claim due watches: %w", err)
	}
	defer rows.Close()

	var due []DueWatch
	for rows.Next() {
		var item DueWatch
		var payload []byte
		var watchID int
		var alias, schedule, lastTriggered string
		if err := rows.Scan(
			&item.ChatID,
			&watchID,
			&alias,
			&payload,
			&schedule,
			&lastTriggered,
		); err != nil {
			return nil, fmt.Errorf("scan due watch: %w", err)
		}
		if err := json.Unmarshal(payload, &item.Watch); err != nil {
			return nil, fmt.Errorf("decode due watch: %w", err)
		}
		item.Watch.ID = watchID
		item.Watch.Alias = alias
		item.Watch.Schedule = schedule
		item.Watch.LastTriggered = lastTriggered
		due = append(due, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read due watches: %w", err)
	}
	return due, nil
}

func (s *Store) initialize() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			chat_id INTEGER PRIMARY KEY,
			next_id INTEGER NOT NULL CHECK (next_id >= 1)
		);

		CREATE TABLE IF NOT EXISTS watches (
			chat_id INTEGER NOT NULL,
			watch_id INTEGER NOT NULL,
			alias TEXT NOT NULL DEFAULT '',
			signature TEXT NOT NULL,
			payload BLOB NOT NULL,
			schedule TEXT NOT NULL DEFAULT '',
			schedule_minute INTEGER,
			last_triggered TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (chat_id, watch_id),
			FOREIGN KEY (chat_id) REFERENCES users(chat_id) ON DELETE CASCADE
		);

		CREATE UNIQUE INDEX IF NOT EXISTS watches_alias
			ON watches(chat_id, alias COLLATE NOCASE)
			WHERE alias <> '';
		CREATE UNIQUE INDEX IF NOT EXISTS watches_signature
			ON watches(chat_id, signature);
		CREATE INDEX IF NOT EXISTS watches_due
			ON watches(schedule_minute)
			WHERE schedule_minute IS NOT NULL;
	`)
	if err != nil {
		return fmt.Errorf("initialize sqlite state: %w", err)
	}
	return nil
}

func (s *Store) migrateLegacyJSON(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy state: %w", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return fmt.Errorf("check sqlite state: %w", err)
	}
	if count > 0 {
		return nil
	}

	var state legacyState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode legacy state: %w", err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin legacy migration: %w", err)
	}
	defer tx.Rollback()

	for chatKey, user := range state.Users {
		chatID, err := strconv.ParseInt(chatKey, 10, 64)
		if err != nil {
			return fmt.Errorf("decode legacy chat ID %q: %w", chatKey, err)
		}
		nextID := user.NextID
		if nextID < 1 {
			nextID = 1
		}
		for i := range user.Watches {
			normalizeWatch(&user.Watches[i])
			if user.Watches[i].ID >= nextID {
				nextID = user.Watches[i].ID + 1
			}
		}
		if _, err := tx.Exec(`INSERT INTO users (chat_id, next_id) VALUES (?, ?)`, chatID, nextID); err != nil {
			return fmt.Errorf("migrate legacy user: %w", err)
		}
		for _, watch := range user.Watches {
			payload, err := encodeWatch(watch)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`
				INSERT INTO watches (
					chat_id, watch_id, alias, signature, payload, schedule, schedule_minute, last_triggered
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			`, chatID, watch.ID, watch.Alias, watchSignature(watch), payload, watch.Schedule, scheduleMinute(watch.Schedule), watch.LastTriggered); err != nil {
				return fmt.Errorf("migrate legacy watch: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy migration: %w", err)
	}
	if err := os.Rename(path, path+".migrated"); err != nil {
		return fmt.Errorf("archive legacy state: %w", err)
	}
	return nil
}

type rowScanner interface {
	Scan(...any) error
}

func queryWatch(row rowScanner) (Watch, error) {
	watch, err := scanWatch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Watch{}, ErrNotFound
	}
	if err != nil {
		return Watch{}, fmt.Errorf("load watch: %w", err)
	}
	return watch, nil
}

func scanWatch(row rowScanner) (Watch, error) {
	var watch Watch
	var payload []byte
	var alias, schedule, lastTriggered string
	if err := row.Scan(&watch.ID, &alias, &payload, &schedule, &lastTriggered); err != nil {
		return Watch{}, err
	}
	if err := json.Unmarshal(payload, &watch); err != nil {
		return Watch{}, fmt.Errorf("decode watch: %w", err)
	}
	watch.Alias = alias
	watch.Schedule = schedule
	watch.LastTriggered = lastTriggered
	return watch, nil
}

func encodeWatch(watch Watch) ([]byte, error) {
	watch.Alias = ""
	watch.Schedule = ""
	watch.LastTriggered = ""
	data, err := json.Marshal(watch)
	if err != nil {
		return nil, fmt.Errorf("encode watch: %w", err)
	}
	return data, nil
}

func normalizeWatch(watch *Watch) {
	watch.Alias = strings.ToLower(strings.TrimSpace(watch.Alias))
	if len(watch.Stops) == 0 && watch.StopCode != "" {
		watch.Stops = []WatchStop{{
			Code:     watch.StopCode,
			Name:     watch.StopName,
			RoadName: watch.RoadName,
		}}
	}
	if len(watch.ServiceNos) == 0 && watch.ServiceNo != "" {
		watch.ServiceNos = []string{strings.ToUpper(watch.ServiceNo)}
	}
	if len(watch.Combinations) == 0 {
		for _, stop := range watch.Stops {
			for _, serviceNo := range watch.ServiceNos {
				watch.Combinations = append(watch.Combinations, WatchCombination{
					StopCode:  stop.Code,
					ServiceNo: strings.ToUpper(serviceNo),
				})
			}
		}
	}
	watch.StopCode = ""
	watch.StopName = ""
	watch.RoadName = ""
	watch.ServiceNo = ""
}

func watchSignature(watch Watch) string {
	normalizeWatch(&watch)
	keys := make([]string, 0, len(watch.Combinations))
	for _, combination := range watch.Combinations {
		keys = append(keys, combinationKey(combination))
	}
	sort.Strings(keys)
	return strings.Join(keys, "\x01")
}

func combinationKey(combination WatchCombination) string {
	return combination.StopCode + "\x00" + strings.ToUpper(combination.ServiceNo)
}

func scheduleMinute(schedule string) any {
	if schedule == "" {
		return nil
	}
	parsed, err := time.Parse("15:04", schedule)
	if err != nil {
		return nil
	}
	return parsed.Hour()*60 + parsed.Minute()
}

func classifyConstraint(err error) error {
	message := err.Error()
	switch {
	case strings.Contains(message, "watches.chat_id, watches.signature"):
		return ErrDuplicate
	case strings.Contains(message, "watches.chat_id, watches.alias"):
		return ErrDuplicateAlias
	default:
		return fmt.Errorf("save watch: %w", err)
	}
}

func persistencePaths(path string) (dbPath, legacyPath string) {
	if strings.EqualFold(filepath.Ext(path), ".json") {
		return strings.TrimSuffix(path, filepath.Ext(path)) + ".db", path
	}
	legacy := strings.TrimSuffix(path, filepath.Ext(path)) + ".json"
	return path, legacy
}
