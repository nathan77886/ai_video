package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenStoreRepairsDanglingVideoReferences(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	state := State{
		Videos: []Video{{ID: "vid_keep"}},
		Shots: []Shot{
			{ID: "E001-S001-SH001", VideoID: "vid_missing"},
			{ID: "E001-S001-SH002", VideoID: "vid_keep"},
		},
		Tasks: []Task{
			{ID: "tsk_missing", VideoID: "vid_missing"},
			{ID: "tsk_keep", VideoID: "vid_keep"},
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.View(func(repaired State) error {
		if repaired.Shots[0].VideoID != "" || repaired.Tasks[0].VideoID != "" {
			t.Fatalf("dangling references remain: %+v", repaired)
		}
		if repaired.Shots[1].VideoID != "vid_keep" || repaired.Tasks[1].VideoID != "vid_keep" {
			t.Fatalf("valid references changed: %+v", repaired)
		}
		if !strings.Contains(strings.Join(repaired.Tasks[0].Logs, "\n"), "已清理悬空关联") {
			t.Fatalf("repair log missing: %+v", repaired.Tasks[0].Logs)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.View(func(repaired State) error {
		if repaired.Shots[0].VideoID != "" || repaired.Tasks[0].VideoID != "" {
			t.Fatalf("repairs were not persisted: %+v", repaired)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
