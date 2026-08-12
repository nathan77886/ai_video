package app

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

type instantProvider struct{}

func (instantProvider) Submit(context.Context, Task) (string, error) {
	return "instant", nil
}

func (instantProvider) Poll(context.Context, string) (ProviderResult, error) {
	return ProviderResult{Status: ProviderSuccess}, nil
}

func (instantProvider) Retrieve(context.Context, string) (DownloadInfo, error) {
	return DownloadInfo{}, nil
}

func TestWorkerResumesQueuedTaskAndPersistsResult(t *testing.T) {
	t.Parallel()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.Update(func(state *State) error {
		state.Projects = append(state.Projects, Project{ID: "prj_test", Name: "test", CreatedAt: now, UpdatedAt: now})
		state.Tasks = append(state.Tasks, Task{
			ID: "tsk_test", ProjectID: "prj_test", Kind: "video_generation",
			Provider: "mock", Model: "T2V-01", Prompt: "test", Duration: 6,
			Resolution: "720P", Status: TaskQueued, MaxAttempts: 3,
			CreatedAt: now, UpdatedAt: now,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	worker := NewWorker(
		store,
		map[string]VideoProvider{"mock": instantProvider{}},
		5*time.Millisecond,
		1<<20,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := worker.Start(ctx, 1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		worker.Wait()
	})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		task, err := worker.getTask("tsk_test")
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == TaskSucceeded {
			if task.Attempts != 1 || task.Progress != 100 {
				t.Fatalf("unexpected completed task: %+v", task)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task did not complete")
}
