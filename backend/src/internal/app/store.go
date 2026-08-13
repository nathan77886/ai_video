package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	mu       sync.RWMutex
	path     string
	mediaDir string
	state    State
}

func OpenStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	mediaDir := filepath.Join(dataDir, "media")
	if err := os.MkdirAll(mediaDir, 0o750); err != nil {
		return nil, fmt.Errorf("create media directory: %w", err)
	}
	s := &Store{path: filepath.Join(dataDir, "state.json"), mediaDir: mediaDir}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	if repairDanglingVideoReferences(&s.state) {
		if err := s.write(s.state); err != nil {
			return nil, fmt.Errorf("persist repaired state: %w", err)
		}
	}
	return s, nil
}

func repairDanglingVideoReferences(state *State) bool {
	repaired := false
	videoIDs := make(map[string]bool, len(state.Videos))
	for _, video := range state.Videos {
		videoIDs[video.ID] = true
	}
	for i := range state.Shots {
		shot := &state.Shots[i]
		if shot.VideoID != "" && !videoIDs[shot.VideoID] {
			shot.VideoID = ""
			repaired = true
		}
	}
	for i := range state.Tasks {
		task := &state.Tasks[i]
		if task.VideoID != "" && !videoIDs[task.VideoID] {
			task.VideoID = ""
			appendTaskLog(task, "本地试片不存在，已清理悬空关联")
			repaired = true
		}
	}
	return repaired
}

func newID(prefix string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(b), nil
}

func (s *Store) MediaDir() string { return s.mediaDir }

func (s *Store) View(fn func(State) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fn(cloneState(s.state))
}

func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.state)
	if err := fn(&next); err != nil {
		return err
	}
	if err := s.write(next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func cloneState(state State) State {
	data, _ := json.Marshal(state)
	var cloned State
	_ = json.Unmarshal(data, &cloned)
	return cloned
}

func (s *Store) write(state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.json")
	if err != nil {
		return fmt.Errorf("create state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o640); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set state permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
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

func findProject(state *State, id string) (*Project, error) {
	for i := range state.Projects {
		if state.Projects[i].ID == id {
			return &state.Projects[i], nil
		}
	}
	return nil, ErrNotFound
}

func findTask(state *State, id string) (*Task, error) {
	for i := range state.Tasks {
		if state.Tasks[i].ID == id {
			return &state.Tasks[i], nil
		}
	}
	return nil, ErrNotFound
}

func findShot(state *State, id string) (*Shot, error) {
	for i := range state.Shots {
		if state.Shots[i].ID == id {
			return &state.Shots[i], nil
		}
	}
	return nil, ErrNotFound
}

func findAsset(state *State, id string) (*Asset, error) {
	for i := range state.Assets {
		if state.Assets[i].ID == id {
			return &state.Assets[i], nil
		}
	}
	return nil, ErrNotFound
}

func normalizeName(value string, max int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len([]rune(value)) > max {
		return "", fmt.Errorf("value must contain 1-%d characters", max)
	}
	return value, nil
}

func appendTaskLog(task *Task, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	task.Logs = append(task.Logs, time.Now().UTC().Format(time.RFC3339)+" "+message)
	if len(task.Logs) > 100 {
		task.Logs = slices.Clone(task.Logs[len(task.Logs)-100:])
	}
}
