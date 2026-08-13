package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

type instantImageProvider struct{}

func (instantImageProvider) Generate(context.Context, Task) ([]byte, string, error) {
	return []byte("png"), "image/png", nil
}

func TestGenerateShotImageSetQueuesImagesWithoutVideoGate(t *testing.T) {
	t.Parallel()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.Update(func(state *State) error {
		state.Projects = append(state.Projects, Project{ID: "prj_test", Name: "test", CreatedAt: now, UpdatedAt: now})
		state.Shots = append(state.Shots, Shot{
			ID: "E001-S001-SH001", ProjectID: "prj_test", AspectRatio: "16:9", Visual: "a valley", Prompt: "wide valley",
			ReviewStatus: ShotPending, GenerationRoute: "post_production", RequiresEditorialSplit: true,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	worker := NewWorker(store, map[string]VideoProvider{}, time.Second, 1<<20, logger)
	worker.SetImageProvider("openai", instantImageProvider{})
	handler := NewHTTPServer(store, worker, logger, 1<<20, false, false, true)
	req := httptest.NewRequest(http.MethodPost, "/api/shots/E001-S001-SH001/images/generate", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result struct {
		Queued  int `json:"queued"`
		Skipped int `json:"skipped"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Queued != 3 || result.Skipped != 0 {
		t.Fatalf("result = %+v, want 3 queued and 0 skipped", result)
	}
	var tasks []Task
	if err := store.View(func(state State) error { tasks = state.Tasks; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Fatalf("image task count = %d, want 3", len(tasks))
	}
	want := map[string]bool{"first-frame": true, "last-frame": true, "preview": true}
	for _, task := range tasks {
		if task.Kind != "image_generation" || task.Provider != "openai" || task.Model != "gpt-image-2" || !want[task.ImageRole] {
			t.Fatalf("unexpected image task: %+v", task)
		}
		delete(want, task.ImageRole)
	}
	if len(want) != 0 {
		t.Fatalf("missing image roles: %v", want)
	}
}

func TestGenerateShotImagesQueuesMissingImages(t *testing.T) {
	t.Parallel()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	firstShot := Shot{ID: "E001-S001-SH001", ProjectID: "prj_test", AspectRatio: "16:9", Visual: "first", Prompt: "first prompt"}
	if err := store.Update(func(state *State) error {
		state.Projects = append(state.Projects, Project{ID: "prj_test", Name: "test", CreatedAt: now, UpdatedAt: now})
		state.Shots = append(state.Shots,
			firstShot,
			Shot{ID: "E001-S001-SH002", ProjectID: "prj_test", AspectRatio: "9:16", Visual: "second", Prompt: "second prompt"},
		)
		state.Tasks = append(state.Tasks, Task{
			ID: "tsk_existing", ProjectID: "prj_test", ShotID: "E001-S001-SH001", Kind: "image_generation",
			Provider: "openai", Model: "gpt-image-2", Prompt: imagePrompt(firstShot, "preview"), ImageRole: "preview", Status: TaskSucceeded, CreatedAt: now, UpdatedAt: now,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	worker := NewWorker(store, map[string]VideoProvider{}, time.Second, 1<<20, logger)
	worker.SetImageProvider("openai", instantImageProvider{})
	handler := NewHTTPServer(store, worker, logger, 1<<20, false, false, true)
	req := httptest.NewRequest(http.MethodPost, "/api/shots/images/generate", bytes.NewBufferString(`{"project_id":"prj_test"}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result struct {
		Queued  int `json:"queued"`
		Skipped int `json:"skipped"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Queued != 5 || result.Skipped != 1 {
		t.Fatalf("result = %+v, want 5 queued and 1 skipped", result)
	}
}

func TestGenerateShotImagesRegeneratesAfterPromptChange(t *testing.T) {
	t.Parallel()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	oldShot := Shot{ID: "E001-S001-SH001", ProjectID: "prj_test", AspectRatio: "16:9", Visual: "old visual", Prompt: "old prompt"}
	newShot := oldShot
	newShot.Visual = "new visual"
	newShot.Prompt = "new prompt"
	if err := store.Update(func(state *State) error {
		state.Projects = append(state.Projects, Project{ID: "prj_test", Name: "test", CreatedAt: now, UpdatedAt: now})
		state.Shots = append(state.Shots, newShot)
		for _, role := range []string{"first-frame", "last-frame", "preview"} {
			state.Tasks = append(state.Tasks, Task{
				ID: "tsk_old_" + role, ProjectID: "prj_test", ShotID: oldShot.ID, Kind: "image_generation",
				Provider: "openai", Model: "gpt-image-2", Prompt: imagePrompt(oldShot, role), ImageRole: role,
				Status: TaskSucceeded, CreatedAt: now, UpdatedAt: now,
			})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tasks, skipped, err := newShotImageTasks(&State{Tasks: mustTasks(t, store)}, newShot)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 || skipped != 0 {
		t.Fatalf("tasks = %d, skipped = %d, want 3 and 0", len(tasks), skipped)
	}
	for _, task := range tasks {
		if task.Prompt != imagePrompt(newShot, task.ImageRole) {
			t.Fatalf("stale prompt queued for %s", task.ImageRole)
		}
	}
}

func mustTasks(t *testing.T, store *Store) []Task {
	t.Helper()
	var tasks []Task
	if err := store.View(func(state State) error { tasks = state.Tasks; return nil }); err != nil {
		t.Fatal(err)
	}
	return tasks
}

func TestGenerateCharacterImagesQueuesMissingImages(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	store, err := OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	characters := []Asset{
		{ID: "ast_character_1", ProjectID: "prj_test", Name: "角色模型 v003 · 蕾拉", Kind: "character", Filename: "char-lela.json", ContentType: "application/json", StoragePath: "prj_test/assets/ast_character_1/char-lela.json", CreatedAt: now},
		{ID: "ast_character_2", ProjectID: "prj_test", Name: "角色模型 v003 · 考尔", Kind: "character", Filename: "char-cawl.json", ContentType: "application/json", StoragePath: "prj_test/assets/ast_character_2/char-cawl.json", CreatedAt: now},
	}
	for _, character := range characters {
		path := filepath.Join(store.MediaDir(), filepath.FromSlash(character.StoragePath))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"name":"test","visual":{"face":"clear"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Update(func(state *State) error {
		state.Projects = append(state.Projects, Project{ID: "prj_test", Name: "test", CreatedAt: now, UpdatedAt: now})
		state.Assets = append(state.Assets, characters...)
		state.Tasks = append(state.Tasks, Task{
			ID: "tsk_existing", ProjectID: "prj_test", AssetID: characters[0].ID, Kind: "image_generation",
			Provider: "openai", Model: "gpt-image-2", ImageRole: "preview", Status: TaskSucceeded, CreatedAt: now, UpdatedAt: now,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	worker := NewWorker(store, map[string]VideoProvider{}, time.Second, 1<<20, logger)
	worker.SetImageProvider("openai", instantImageProvider{})
	handler := NewHTTPServer(store, worker, logger, 1<<20, false, false, true)
	req := httptest.NewRequest(http.MethodPost, "/api/assets/characters/images/generate", bytes.NewBufferString(`{"project_id":"prj_test"}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result struct {
		Characters int `json:"characters"`
		Queued     int `json:"queued"`
		Skipped    int `json:"skipped"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Characters != 2 || result.Queued != 7 || result.Skipped != 1 {
		t.Fatalf("result = %+v, want 2 characters, 7 queued, 1 skipped", result)
	}
	if err := store.View(func(state State) error {
		for _, task := range state.Tasks[1:] {
			if task.AssetID == "" || task.ShotID != "" || task.Kind != "image_generation" || task.MaxAttempts != 2 {
				t.Fatalf("unexpected character image task: %+v", task)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteCharacterImageLinksAsset(t *testing.T) {
	t.Parallel()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	character := Asset{ID: "ast_character", ProjectID: "prj_test", Name: "角色模型 v003 · 蕾拉", Kind: "character"}
	task := Task{
		ID: "tsk_character", ProjectID: "prj_test", AssetID: character.ID, Kind: "image_generation",
		Provider: "openai", Model: "gpt-image-2", ImageRole: "preview", Status: TaskRunning, MaxAttempts: 2,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.Update(func(state *State) error {
		state.Assets = append(state.Assets, character)
		state.Tasks = append(state.Tasks, task)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(store, map[string]VideoProvider{}, time.Second, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := worker.completeImage(task.ID, []byte("png"), "image/png"); err != nil {
		t.Fatal(err)
	}
	if err := store.View(func(state State) error {
		if len(state.Assets) != 2 || len(state.AssetLinks) != 1 {
			t.Fatalf("assets = %d, links = %d", len(state.Assets), len(state.AssetLinks))
		}
		link := state.AssetLinks[0]
		if link.TargetType != "asset" || link.TargetID != character.ID || link.AssetID != state.Assets[1].ID {
			t.Fatalf("unexpected character image link: %+v", link)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
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
			Provider: "mock", Model: "MiniMax-H3", Prompt: "test", Duration: 6,
			Resolution: "768P", AspectRatio: "16:9", Status: TaskQueued, MaxAttempts: 3,
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

func TestMiniMaxProviderV2SubmitAndPoll(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/v2/video_generation":
			if r.Method != http.MethodPost {
				t.Errorf("method = %s", r.Method)
			}
			var payload struct {
				Model      string `json:"model"`
				Duration   int    `json:"duration"`
				Resolution string `json:"resolution"`
				Ratio      string `json:"ratio"`
				Content    []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					Role     string `json:"role"`
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Model != "MiniMax-H3" || payload.Duration != 6 || payload.Resolution != "768P" || payload.Ratio != "16:9" || len(payload.Content) != 2 || payload.Content[0].Text != "test prompt" || payload.Content[1].Role != "first_frame" || payload.Content[1].ImageURL.URL != "data:image/png;base64,cG5n" {
				t.Errorf("unexpected payload: %+v", payload)
			}
			_, _ = io.WriteString(w, "{\"task_id\":\"upstream-1\"}")
		case "/v2/query/video_generation/upstream-1":
			_, _ = io.WriteString(w, "{\"task\":{\"status\":\"succeeded\",\"content\":{\"url\":\"https://files.example/video.mp4\"}}}")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider, err := NewMiniMaxProvider(server.URL, "test-key", true)
	if err != nil {
		t.Fatal(err)
	}
	id, err := provider.Submit(t.Context(), Task{
		Model: "MiniMax-H3", Prompt: "test prompt", Duration: 6, Resolution: "768P", AspectRatio: "16:9",
		VideoInputs: []VideoInput{{Role: "first_frame", ContentType: "image/png", Data: []byte("png")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "upstream-1" {
		t.Fatalf("task id = %q", id)
	}
	result, err := provider.Poll(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != ProviderSuccess || result.DownloadURL != "https://files.example/video.mp4" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestWorkerPrepareVideoTaskUsesSelectedInputs(t *testing.T) {
	t.Parallel()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	modelPath := filepath.Join(store.MediaDir(), "prj_test/assets/ast_character/character.json")
	framePath := filepath.Join(store.MediaDir(), "prj_test/assets/ast_frame/first-frame.png")
	lastFramePath := filepath.Join(store.MediaDir(), "prj_test/assets/ast_last/last-frame.png")
	for path, content := range map[string]string{modelPath: `{"name":"Lela"}`, framePath: "first", lastFramePath: "last"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Update(func(state *State) error {
		state.Shots = append(state.Shots, Shot{ID: "E001-S001-SH001", ProjectID: "prj_test", SourceMode: "first_last_frame"})
		state.Assets = append(state.Assets,
			Asset{ID: "ast_character", ProjectID: "prj_test", Kind: "character", Name: "蕾拉", StoragePath: "prj_test/assets/ast_character/character.json"},
			Asset{ID: "ast_frame", ProjectID: "prj_test", Kind: "image", ContentType: "image/png", Size: 5, StoragePath: "prj_test/assets/ast_frame/first-frame.png"},
			Asset{ID: "ast_last", ProjectID: "prj_test", Kind: "image", ContentType: "image/png", Size: 4, StoragePath: "prj_test/assets/ast_last/last-frame.png"},
		)
		state.AssetLinks = append(state.AssetLinks,
			AssetLink{ID: "lnk_character", ProjectID: "prj_test", AssetID: "ast_character", TargetType: "shot", TargetID: "E001-S001-SH001", CreatedAt: now},
			AssetLink{ID: "lnk_frame", ProjectID: "prj_test", AssetID: "ast_frame", TargetType: "shot", TargetID: "E001-S001-SH001", Note: "GPT Image 2 首帧图", CreatedAt: now},
			AssetLink{ID: "lnk_last", ProjectID: "prj_test", AssetID: "ast_last", TargetType: "shot", TargetID: "E001-S001-SH001", Note: "GPT Image 2 末帧图", CreatedAt: now},
		)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(store, map[string]VideoProvider{}, time.Second, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
	task, err := worker.prepareVideoTask(Task{ShotID: "E001-S001-SH001", Prompt: "scene", UseFrameImages: true, CharacterPromptCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(task.VideoInputs) != 2 || task.VideoInputs[0].Role != "first_frame" || task.VideoInputs[1].Role != "last_frame" || !strings.Contains(task.Prompt, `"name":"Lela"`) {
		t.Fatalf("unexpected prepared task: %+v", task)
	}
}

func TestGenerateShotRejectsFrameAndReferenceImages(t *testing.T) {
	t.Parallel()
	store, worker, _ := newShotTestServer(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHTTPServer(store, worker, logger, 1<<20, true, true, false)
	now := time.Now().UTC()
	if err := store.Update(func(state *State) error {
		state.Projects = append(state.Projects, Project{ID: "prj_test", Name: "test", CreatedAt: now, UpdatedAt: now})
		state.Shots = append(state.Shots, Shot{
			ID: "E001-S001-SH001", ProjectID: "prj_test", EpisodeID: "E001", SceneID: "E001-S001",
			Chapter: 1, ChapterTitle: "chapter", Visual: "visual", Duration: 6, AspectRatio: "16:9",
			TargetModel: "MiniMax-H3", GenerationRoute: "video_api", Prompt: "prompt", ReviewStatus: ShotApproved,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/shots/E001-S001-SH001/generate", bytes.NewBufferString(`{"provider":"mock","use_frame_images":true,"reference_image_ids":["ast_1"]}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestWorkerPrepareVideoTaskUsesSelectedReferenceImages(t *testing.T) {
	t.Parallel()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(store.MediaDir(), "prj_test/assets/ast_reference/reference.png")
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, []byte("reference"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(state *State) error {
		state.Shots = append(state.Shots, Shot{ID: "E001-S001-SH001", ProjectID: "prj_test"})
		state.Assets = append(state.Assets, Asset{ID: "ast_reference", ProjectID: "prj_test", Kind: "image", ContentType: "image/png", Size: 9, StoragePath: "prj_test/assets/ast_reference/reference.png"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(store, map[string]VideoProvider{}, time.Second, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
	task, err := worker.prepareVideoTask(Task{ProjectID: "prj_test", ShotID: "E001-S001-SH001", ReferenceImageIDs: []string{"ast_reference"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(task.VideoInputs) != 1 || task.VideoInputs[0].Role != "reference_image" || string(task.VideoInputs[0].Data) != "reference" {
		t.Fatalf("unexpected reference inputs: %+v", task.VideoInputs)
	}
}

func TestGenerateShotSequenceQueuesOnlyFirstTask(t *testing.T) {
	t.Parallel()
	store, worker, _ := newShotTestServer(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHTTPServer(store, worker, logger, 1<<20, true, true, false)
	now := time.Now().UTC()
	if err := store.Update(func(state *State) error {
		state.Projects = append(state.Projects, Project{ID: "prj_test", Name: "test", CreatedAt: now, UpdatedAt: now})
		for _, id := range []string{"E001-S001-SH001", "E001-S001-SH002"} {
			state.Shots = append(state.Shots, Shot{
				ID: id, ProjectID: "prj_test", EpisodeID: "E001", SceneID: "E001-S001", Chapter: 1,
				ChapterTitle: "chapter", Visual: "visual", Duration: 6, AspectRatio: "16:9", TargetModel: "MiniMax-H3",
				GenerationRoute: "video_api", Prompt: "prompt", InputVersion: "v009/v007", ReviewStatus: ShotApproved,
			})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/shots/generate-sequence", bytes.NewBufferString(`{"project_id":"prj_test","shot_ids":["E001-S001-SH001","E001-S001-SH002"],"provider":"minimax","resolution":"768P","character_prompt_count":1}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if err := store.View(func(state State) error {
		if len(state.Tasks) != 2 || state.Tasks[0].PreviousTaskID != "" || state.Tasks[1].PreviousTaskID != state.Tasks[0].ID || state.Tasks[1].CharacterPromptCount != 1 || state.Tasks[0].InputVersion != "v009/v007" || state.Tasks[1].InputVersion != "v009/v007" {
			t.Fatalf("unexpected sequence tasks: %+v", state.Tasks)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateShotSequenceAcceptsSucceededPreviousVideoAndPromptOverride(t *testing.T) {
	t.Parallel()
	store, worker, _ := newShotTestServer(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHTTPServer(store, worker, logger, 1<<20, true, true, false)
	now := time.Now().UTC()
	if err := store.Update(func(state *State) error {
		state.Projects = append(state.Projects, Project{ID: "prj_test", Name: "test", CreatedAt: now, UpdatedAt: now})
		for _, shot := range []Shot{
			{ID: "E001-S001-SH001", ProjectID: "prj_test", EpisodeID: "E001", SceneID: "E001-S001", Chapter: 1, ChapterTitle: "chapter", Visual: "visual", Duration: 6, AspectRatio: "16:9", TargetModel: "MiniMax-H3", GenerationRoute: "video_api", Prompt: "original", ReviewStatus: ShotApproved},
			{ID: "E001-S001-SH002", ProjectID: "prj_test", EpisodeID: "E001", SceneID: "E001-S001", Chapter: 1, ChapterTitle: "chapter", Visual: "visual", Duration: 6, AspectRatio: "16:9", TargetModel: "MiniMax-H3", GenerationRoute: "video_api", Prompt: "second", ReviewStatus: ShotApproved},
		} {
			state.Shots = append(state.Shots, shot)
		}
		state.Tasks = append(state.Tasks, Task{ID: "tsk_previous", ProjectID: "prj_test", ShotID: "E001-S000-SH001", Status: TaskSucceeded, VideoID: "vid_previous"})
		state.Videos = append(state.Videos, Video{ID: "vid_previous", ProjectID: "prj_test", TaskID: "tsk_previous"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/shots/generate-sequence", bytes.NewBufferString(`{"project_id":"prj_test","shot_ids":["E001-S001-SH001","E001-S001-SH002"],"previous_task_id":"tsk_previous","prompt_overrides":{"E001-S001-SH001":"revised prompt"},"provider":"minimax"}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if err := store.View(func(state State) error {
		created := state.Tasks[len(state.Tasks)-2:]
		if created[0].PreviousTaskID != "tsk_previous" || created[0].Prompt != "revised prompt" || created[1].PreviousTaskID != created[0].ID {
			t.Fatalf("unexpected tasks: %+v", created)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAIImageProviderGenerate(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/images/generations" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		var payload struct {
			Model        string `json:"model"`
			Prompt       string `json:"prompt"`
			Size         string `json:"size"`
			Quality      string `json:"quality"`
			OutputFormat string `json:"output_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != "gpt-image-2" || !strings.Contains(payload.Prompt, "PG-13 cinematic still") || !strings.HasSuffix(payload.Prompt, "scene") || payload.Size != "1024x1536" || payload.Quality != "low" || payload.OutputFormat != "png" {
			t.Fatalf("unexpected payload: %+v", payload)
		}
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"`+base64.StdEncoding.EncodeToString([]byte("png"))+`"}]}`)
	}))
	defer server.Close()
	provider, err := NewOpenAIImageProvider(server.URL+"/v1", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	image, contentType, err := provider.Generate(t.Context(), Task{Prompt: "scene", AspectRatio: "9:16"})
	if err != nil {
		t.Fatal(err)
	}
	if string(image) != "png" || contentType != "image/png" {
		t.Fatalf("result = %q, %q", image, contentType)
	}
}

func TestImageSafetyPrompt(t *testing.T) {
	t.Parallel()
	prompt := imageSafetyPrompt("a dark fantasy scene")
	for _, required := range []string{"PG-13 cinematic still", "No injury", "a dark fantasy scene"} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt missing %q: %s", required, prompt)
		}
	}
}

func TestImageGenerationPromptUsesCharacterSafety(t *testing.T) {
	t.Parallel()
	prompt := imageGenerationPrompt(Task{AssetID: "ast_character", Prompt: "clean character turnaround"})
	for _, required := range []string{"character design artwork", "No injury", "clean character turnaround"} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt missing %q: %s", required, prompt)
		}
	}
	if strings.Contains(prompt, "tense, non-graphic aftermath") {
		t.Fatalf("character prompt contains shot-only direction: %s", prompt)
	}
}

func TestNewOpenAIImageProvider_UsesImageTimeout(t *testing.T) {
	t.Parallel()
	provider, err := NewOpenAIImageProvider("http://127.0.0.1/v1", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if provider.client.Timeout != imageRequestTimeout {
		t.Fatalf("image timeout = %s, want %s", provider.client.Timeout, imageRequestTimeout)
	}
}

func TestWorkerRetry_AllowsImageTaskWithoutVideoGate(t *testing.T) {
	t.Parallel()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shot := Shot{ID: "E001-S001-SH001", ProjectID: "prj_test", ReviewStatus: ShotPending, GenerationRoute: "post_production"}
	task := Task{ID: "tsk_test", ProjectID: shot.ProjectID, ShotID: shot.ID, Kind: "image_generation", Provider: "openai", Status: TaskFailed, MaxAttempts: 2}
	if err := store.Update(func(state *State) error {
		state.Shots = append(state.Shots, shot)
		state.Tasks = append(state.Tasks, task)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(store, map[string]VideoProvider{}, time.Second, 1, slog.Default())
	if err := worker.Retry(task.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := worker.getTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != TaskQueued {
		t.Fatalf("status = %s, want %s", stored.Status, TaskQueued)
	}
}

func TestWorkerRetry_ResetsExhaustedImageAttempts(t *testing.T) {
	t.Parallel()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shot := Shot{ID: "E001-S001-SH001", ProjectID: "prj_test", ReviewStatus: ShotPending, GenerationRoute: "post_production"}
	task := Task{
		ID: "tsk_test", ProjectID: shot.ProjectID, ShotID: shot.ID, Kind: "image_generation", Provider: "openai",
		Status: TaskFailed, Attempts: 2, MaxAttempts: 2, Error: "retry limit reached (2 attempts)",
	}
	if err := store.Update(func(state *State) error {
		state.Shots = append(state.Shots, shot)
		state.Tasks = append(state.Tasks, task)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(store, map[string]VideoProvider{}, time.Second, 1, slog.Default())
	if err := worker.Retry(task.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := worker.getTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != TaskQueued || stored.Attempts != 0 || stored.Error != "" {
		t.Fatalf("task after retry = %+v", stored)
	}
	if !strings.Contains(strings.Join(stored.Logs, "\n"), "手动重试，重置图片自动重试计数") {
		t.Fatalf("retry reset was not logged: %v", stored.Logs)
	}
}

func TestIsTextAsset(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		asset Asset
		want  bool
	}{
		{name: "json extension", asset: Asset{Filename: "data.json"}, want: true},
		{name: "JSON content type", asset: Asset{Filename: "data", ContentType: "application/json"}, want: true},
		{name: "markdown", asset: Asset{Filename: "README.md"}, want: true},
		{name: "video", asset: Asset{Filename: "draft.mp4", ContentType: "video/mp4"}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isTextAsset(test.asset); got != test.want {
				t.Fatalf("isTextAsset(%+v) = %t, want %t", test.asset, got, test.want)
			}
		})
	}
}

func TestShotGenerationGates(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		status ShotReviewStatus
		route  string
		split  bool
		want   int
	}{
		{name: "pending", status: ShotPending, route: "video_api", want: http.StatusConflict},
		{name: "post production", status: ShotApproved, route: "post_production", want: http.StatusConflict},
		{name: "editorial split", status: ShotApproved, route: "video_api", split: true, want: http.StatusConflict},
		{name: "approved video api", status: ShotApproved, route: "video_api", want: http.StatusAccepted},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, worker, handler := newShotTestServer(t)
			now := time.Now().UTC()
			if err := store.Update(func(state *State) error {
				state.Projects = append(state.Projects, Project{ID: "prj_test", Name: "test", CreatedAt: now, UpdatedAt: now})
				state.Shots = append(state.Shots, Shot{
					ID: "E001-S001-SH001", ProjectID: "prj_test", EpisodeID: "E001", SceneID: "E001-S001",
					Chapter: 1, ChapterTitle: "chapter", Visual: "visual", Duration: 6, AspectRatio: "16:9",
					TargetModel: "MiniMax-H3", GenerationRoute: test.route, RequiresEditorialSplit: test.split,
					Prompt: "prompt", ReviewStatus: test.status, CreatedAt: now, UpdatedAt: now,
				})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/shots/E001-S001-SH001/generate", bytes.NewBufferString(`{"provider":"mock","resolution":"768P"}`))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, test.want, response.Body.String())
			}
			if test.want == http.StatusAccepted {
				var tasks []Task
				if err := store.View(func(state State) error { tasks = state.Tasks; return nil }); err != nil {
					t.Fatal(err)
				}
				if len(tasks) != 1 || tasks[0].ShotID != "E001-S001-SH001" {
					t.Fatalf("unexpected tasks: %+v", tasks)
				}
				_ = worker
			}
		})
	}
}

func TestShotImportSkipsExistingReview(t *testing.T) {
	t.Parallel()
	store, _, handler := newShotTestServer(t)
	now := time.Now().UTC()
	if err := store.Update(func(state *State) error {
		state.Projects = append(state.Projects, Project{ID: "prj_test", Name: "test", CreatedAt: now, UpdatedAt: now})
		state.Shots = append(state.Shots, Shot{
			ID: "E001-S001-SH001", ProjectID: "prj_test", EpisodeID: "E001", SceneID: "E001-S001",
			Chapter: 1, ChapterTitle: "old", Visual: "old", Duration: 6, AspectRatio: "16:9",
			TargetModel: "MiniMax-H3", GenerationRoute: "video_api", Prompt: "old",
			ReviewStatus: ShotApproved, ReviewNote: "keep", CreatedAt: now, UpdatedAt: now,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	body := `{"project_id":"prj_test","shots":[{"id":"E001-S001-SH001","episode_id":"E001","scene_id":"E001-S001","chapter":1,"chapter_title":"new","visual":"new","duration_sec":6,"aspect_ratio":"16:9","target_model":"MiniMax-H3","generation_route":"video_api","prompt":"new"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/shots/import", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if err := store.View(func(state State) error {
		if len(state.Shots) != 1 || state.Shots[0].ReviewStatus != ShotApproved || state.Shots[0].ReviewNote != "keep" || state.Shots[0].Prompt != "old" {
			t.Fatalf("existing shot overwritten: %+v", state.Shots)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestShotImportReplacesExistingContentForReview(t *testing.T) {
	t.Parallel()
	store, _, handler := newShotTestServer(t)
	now := time.Now().UTC()
	if err := store.Update(func(state *State) error {
		state.Projects = append(state.Projects, Project{ID: "prj_test", Name: "test", CreatedAt: now, UpdatedAt: now})
		state.Shots = append(state.Shots, Shot{
			ID: "E001-S001-SH001", ProjectID: "prj_test", EpisodeID: "E001", SceneID: "E001-S001",
			Chapter: 1, ChapterTitle: "old", Visual: "old", Duration: 6, AspectRatio: "16:9",
			TargetModel: "MiniMax-H3", GenerationRoute: "video_api", Prompt: "old",
			ReviewStatus: ShotApproved, ReviewNote: "keep", TaskID: "tsk_old", VideoID: "vid_old",
			CreatedAt: now, UpdatedAt: now,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	body := `{"project_id":"prj_test","replace_existing":true,"shots":[{"id":"E001-S001-SH001","episode_id":"E001","scene_id":"E001-S001","chapter":1,"chapter_title":"new","visual":"new","duration_sec":5,"aspect_ratio":"16:9","target_model":"MiniMax-H3","generation_route":"video_api","prompt":"new","negative_prompt":"negative","input_version":"v009/v007"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/shots/import", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result map[string]int
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["updated"] != 1 || result["imported"] != 0 || result["skipped"] != 0 {
		t.Fatalf("result = %+v", result)
	}
	if err := store.View(func(state State) error {
		shot := state.Shots[0]
		if len(state.Shots) != 1 || shot.ReviewStatus != ShotPending || shot.ReviewNote != "" || shot.Prompt != "new" || shot.NegativePrompt != "negative" || shot.Duration != 5 || shot.InputVersion != "v009/v007" || shot.TaskID != "tsk_old" || shot.VideoID != "vid_old" || !shot.CreatedAt.Equal(now) {
			t.Fatalf("unexpected replacement: %+v", shot)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestListVideosSortsByShotID(t *testing.T) {
	t.Parallel()
	store, _, handler := newShotTestServer(t)
	now := time.Now().UTC()
	if err := store.Update(func(state *State) error {
		state.Videos = append(state.Videos,
			Video{ID: "vid_3", ProjectID: "prj_test", ShotID: "E001-S002-SH001", CreatedAt: now.Add(3 * time.Second)},
			Video{ID: "vid_2", ProjectID: "prj_test", ShotID: "E001-S001-SH002", CreatedAt: now.Add(2 * time.Second)},
			Video{ID: "vid_1", ProjectID: "prj_test", ShotID: "E001-S001-SH001", CreatedAt: now.Add(time.Second)},
			Video{ID: "vid_upload", ProjectID: "prj_test", CreatedAt: now.Add(4 * time.Second)},
		)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/videos?project_id=prj_test", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var videos []Video
	if err := json.NewDecoder(response.Body).Decode(&videos); err != nil {
		t.Fatal(err)
	}
	want := []string{"vid_1", "vid_2", "vid_3", "vid_upload"}
	if len(videos) != len(want) {
		t.Fatalf("video count = %d, want %d", len(videos), len(want))
	}
	for i, id := range want {
		if videos[i].ID != id {
			t.Fatalf("videos[%d] = %q, want %q", i, videos[i].ID, id)
		}
	}
}

func TestReviewVideo(t *testing.T) {
	t.Parallel()
	store, _, handler := newShotTestServer(t)
	now := time.Now().UTC()
	if err := store.Update(func(state *State) error {
		state.Videos = append(state.Videos, Video{
			ID: "vid_test", ProjectID: "prj_test", Filename: "test.mp4",
			ReviewStatus: VideoUnreviewed, CreatedAt: now,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(
		http.MethodPatch,
		"/api/videos/vid_test/review",
		bytes.NewBufferString(`{"status":"usable","note":"keep this take"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if err := store.View(func(state State) error {
		video := state.Videos[0]
		if video.ReviewStatus != VideoUsable || video.ReviewNote != "keep this take" || video.ReviewUpdatedAt == nil {
			t.Fatalf("unexpected review: %+v", video)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReviewVideoRejectsInvalidStatus(t *testing.T) {
	t.Parallel()
	_, _, handler := newShotTestServer(t)
	req := httptest.NewRequest(
		http.MethodPatch,
		"/api/videos/vid_test/review",
		bytes.NewBufferString(`{"status":"approved"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body = %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
}

func TestDeleteVideoClearsShotAndTaskReferences(t *testing.T) {
	t.Parallel()
	store, _, handler := newShotTestServer(t)
	now := time.Now().UTC()
	if err := store.Update(func(state *State) error {
		state.Videos = append(state.Videos, Video{
			ID: "vid_test", ProjectID: "prj_test", Filename: "test.mp4",
			StoragePath: "prj_test/videos/vid_test/test.mp4", CreatedAt: now,
		})
		state.Shots = append(state.Shots, Shot{
			ID: "E001-S001-SH001", ProjectID: "prj_test", VideoID: "vid_test",
		})
		state.Tasks = append(state.Tasks, Task{
			ID: "tsk_test", ProjectID: "prj_test", VideoID: "vid_test",
			Status: TaskSucceeded, UpdatedAt: now,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/videos/vid_test", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if err := store.View(func(state State) error {
		if len(state.Videos) != 0 || state.Shots[0].VideoID != "" || state.Tasks[0].VideoID != "" {
			t.Fatalf("dangling references remain: %+v", state)
		}
		if !strings.Contains(strings.Join(state.Tasks[0].Logs, "\n"), "关联试片已删除") {
			t.Fatalf("task log missing deletion: %+v", state.Tasks[0].Logs)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func newShotTestServer(t *testing.T) (*Store, *Worker, http.Handler) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	worker := NewWorker(store, map[string]VideoProvider{"mock": MockProvider{}}, 5*time.Millisecond, 1<<20, logger)
	return store, worker, NewHTTPServer(store, worker, logger, 1<<20, false, false, false)
}
