package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound  = errors.New("watch item not found")
	ErrDuplicate = errors.New("watch item already exists")
)

type Watch struct {
	ID            int                `json:"id"`
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

type state struct {
	Users map[string]*User `json:"users"`
}

type Store struct {
	mu    sync.RWMutex
	path  string
	state state
}

type DueWatch struct {
	ChatID int64
	Watch  Watch
}

func Open(path string) (*Store, error) {
	s := &Store{
		path: path,
		state: state{
			Users: make(map[string]*User),
		},
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	if s.state.Users == nil {
		s.state.Users = make(map[string]*User)
	}
	for _, user := range s.state.Users {
		for i := range user.Watches {
			normalizeWatch(&user.Watches[i])
		}
	}
	return s, nil
}

func (s *Store) Add(chatID int64, watch Watch) (Watch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalizeWatch(&watch)
	user := s.user(chatID)
	for _, existing := range user.Watches {
		if sameWatch(existing, watch) {
			return Watch{}, ErrDuplicate
		}
	}
	if user.NextID < 1 {
		user.NextID = 1
	}
	watch.ID = user.NextID
	user.NextID++
	user.Watches = append(user.Watches, watch)
	if err := s.saveLocked(); err != nil {
		user.Watches = user.Watches[:len(user.Watches)-1]
		user.NextID--
		return Watch{}, err
	}
	return watch, nil
}

func (s *Store) List(chatID int64) []Watch {
	s.mu.RLock()
	defer s.mu.RUnlock()

	user := s.state.Users[key(chatID)]
	if user == nil {
		return nil
	}
	watches := append([]Watch(nil), user.Watches...)
	sort.Slice(watches, func(i, j int) bool { return watches[i].ID < watches[j].ID })
	return watches
}

func (s *Store) Get(chatID int64, id int) (Watch, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	user := s.state.Users[key(chatID)]
	if user != nil {
		for _, watch := range user.Watches {
			if watch.ID == id {
				return watch, nil
			}
		}
	}
	return Watch{}, ErrNotFound
}

func (s *Store) Delete(chatID int64, id int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	user := s.state.Users[key(chatID)]
	if user == nil {
		return ErrNotFound
	}
	for i, watch := range user.Watches {
		if watch.ID != id {
			continue
		}
		removed := watch
		user.Watches = append(user.Watches[:i], user.Watches[i+1:]...)
		if err := s.saveLocked(); err != nil {
			user.Watches = append(user.Watches, Watch{})
			copy(user.Watches[i+1:], user.Watches[i:])
			user.Watches[i] = removed
			return err
		}
		return nil
	}
	return ErrNotFound
}

func (s *Store) SetSchedule(chatID int64, id int, schedule string) (Watch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	user := s.state.Users[key(chatID)]
	if user == nil {
		return Watch{}, ErrNotFound
	}
	for i := range user.Watches {
		if user.Watches[i].ID != id {
			continue
		}
		oldSchedule := user.Watches[i].Schedule
		oldLastTriggered := user.Watches[i].LastTriggered
		user.Watches[i].Schedule = schedule
		user.Watches[i].LastTriggered = ""
		if err := s.saveLocked(); err != nil {
			user.Watches[i].Schedule = oldSchedule
			user.Watches[i].LastTriggered = oldLastTriggered
			return Watch{}, err
		}
		return user.Watches[i], nil
	}
	return Watch{}, ErrNotFound
}

func (s *Store) ClaimDue(now time.Time) ([]DueWatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	minute := now.Format("2006-01-02 15:04")
	hhmm := now.Format("15:04")
	var due []DueWatch
	type claimedWatch struct {
		watch         *Watch
		lastTriggered string
	}
	var claimed []claimedWatch
	for chatKey, user := range s.state.Users {
		chatID, err := strconv.ParseInt(chatKey, 10, 64)
		if err != nil {
			continue
		}
		for i := range user.Watches {
			watch := &user.Watches[i]
			if watch.Schedule != hhmm || watch.LastTriggered == minute {
				continue
			}
			claimed = append(claimed, claimedWatch{watch: watch, lastTriggered: watch.LastTriggered})
			watch.LastTriggered = minute
			due = append(due, DueWatch{ChatID: chatID, Watch: *watch})
		}
	}
	if len(due) > 0 {
		if err := s.saveLocked(); err != nil {
			for _, item := range claimed {
				item.watch.LastTriggered = item.lastTriggered
			}
			return nil, err
		}
	}
	return due, nil
}

func (s *Store) user(chatID int64) *User {
	k := key(chatID)
	user := s.state.Users[k]
	if user == nil {
		user = &User{NextID: 1, Watches: []Watch{}}
		s.state.Users[k] = user
	}
	return user
}

func normalizeWatch(watch *Watch) {
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

func sameWatch(left, right Watch) bool {
	normalizeWatch(&left)
	normalizeWatch(&right)
	if len(left.Combinations) != len(right.Combinations) {
		return false
	}
	rightCombinations := make(map[string]bool, len(right.Combinations))
	for _, combination := range right.Combinations {
		rightCombinations[combinationKey(combination)] = true
	}
	for _, combination := range left.Combinations {
		if !rightCombinations[combinationKey(combination)] {
			return false
		}
	}
	return true
}

func combinationKey(combination WatchCombination) string {
	return combination.StopCode + "\x00" + strings.ToUpper(combination.ServiceNo)
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("secure temporary state: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

func key(chatID int64) string {
	return strconv.FormatInt(chatID, 10)
}
