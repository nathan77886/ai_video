package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	maxJSONBody         = 2 << 20
	maxTextPreviewBytes = 1 << 20
)

var shotIDPattern = regexp.MustCompile(`^E\d{3}-S\d{3}-SH\d{3}$`)

type HTTPServer struct {
	store             *Store
	worker            *Worker
	logger            *slog.Logger
	maxUploadBytes    int64
	paidAllowed       bool
	miniMaxConfigured bool
	openAIConfigured  bool
}

func NewHTTPServer(
	store *Store,
	worker *Worker,
	logger *slog.Logger,
	maxUploadBytes int64,
	paidAllowed bool,
	miniMaxConfigured bool,
	openAIConfigured bool,
) http.Handler {
	s := &HTTPServer{
		store: store, worker: worker, logger: logger, maxUploadBytes: maxUploadBytes,
		paidAllowed: paidAllowed, miniMaxConfigured: miniMaxConfigured, openAIConfigured: openAIConfigured,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/config", s.config)
	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("POST /api/projects", s.createProject)
	mux.HandleFunc("PATCH /api/projects/{id}", s.updateProject)
	mux.HandleFunc("DELETE /api/projects/{id}", s.deleteProject)
	mux.HandleFunc("GET /api/scripts", s.listScripts)
	mux.HandleFunc("POST /api/scripts", s.createScript)
	mux.HandleFunc("PATCH /api/scripts/{id}", s.updateScript)
	mux.HandleFunc("DELETE /api/scripts/{id}", s.deleteScript)
	mux.HandleFunc("GET /api/shots", s.listShots)
	mux.HandleFunc("POST /api/shots/import", s.importShots)
	mux.HandleFunc("PATCH /api/shots/{id}/review", s.reviewShot)
	mux.HandleFunc("POST /api/shots/{id}/generate", s.generateShot)
	mux.HandleFunc("POST /api/shots/generate-sequence", s.generateShotSequence)
	mux.HandleFunc("POST /api/shots/{id}/images/generate", s.generateShotImageSet)
	mux.HandleFunc("POST /api/shots/images/generate", s.generateShotImages)
	mux.HandleFunc("POST /api/shots/{id}/assets", s.linkShotAsset)
	mux.HandleFunc("DELETE /api/shots/{id}/assets/{linkID}", s.unlinkShotAsset)
	mux.HandleFunc("GET /api/assets", s.listAssets)
	mux.HandleFunc("POST /api/assets", s.uploadAsset)
	mux.HandleFunc("POST /api/assets/characters/images/generate", s.generateCharacterImages)
	mux.HandleFunc("GET /api/assets/{id}/content", s.assetContent)
	mux.HandleFunc("GET /api/assets/{id}/preview", s.assetPreview)
	mux.HandleFunc("POST /api/assets/{id}/links", s.createAssetLink)
	mux.HandleFunc("DELETE /api/assets/{id}/links/{linkID}", s.deleteAssetLink)
	mux.HandleFunc("DELETE /api/assets/{id}", s.deleteAsset)
	mux.HandleFunc("GET /api/videos", s.listVideos)
	mux.HandleFunc("POST /api/videos", s.uploadVideo)
	mux.HandleFunc("PATCH /api/videos/{id}/review", s.reviewVideo)
	mux.HandleFunc("GET /api/videos/{id}/content", s.videoContent)
	mux.HandleFunc("DELETE /api/videos/{id}", s.deleteVideo)
	mux.HandleFunc("GET /api/tasks", s.listTasks)
	mux.HandleFunc("POST /api/tasks", s.createTask)
	mux.HandleFunc("POST /api/tasks/{id}/cancel", s.cancelTask)
	mux.HandleFunc("POST /api/tasks/{id}/retry", s.retryTask)
	return s.middleware(mux)
}

func (s *HTTPServer) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("panic in HTTP handler", "panic", recovered, "method", r.Method, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
			s.logger.Info("http request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *HTTPServer) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *HTTPServer) config(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"paid_generation_allowed": s.paidAllowed,
		"minimax_configured":      s.miniMaxConfigured,
		"openai_image_configured": s.openAIConfigured,
		"max_upload_bytes":        s.maxUploadBytes,
		"providers":               []string{"mock", "minimax"},
	})
}

func (s *HTTPServer) listProjects(w http.ResponseWriter, _ *http.Request) {
	var projects []Project
	_ = s.store.View(func(state State) error {
		projects = state.Projects
		slices.SortFunc(projects, func(a, b Project) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
		return nil
	})
	writeJSON(w, http.StatusOK, projects)
}

func (s *HTTPServer) createProject(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name, err := normalizeName(input.Name, 100)
	if err != nil {
		writeError(w, http.StatusBadRequest, "name "+err.Error())
		return
	}
	if len([]rune(input.Description)) > 1000 {
		writeError(w, http.StatusBadRequest, "description exceeds 1000 characters")
		return
	}
	id, err := newID("prj")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate project id")
		return
	}
	now := time.Now().UTC()
	project := Project{ID: id, Name: name, Description: strings.TrimSpace(input.Description), CreatedAt: now, UpdatedAt: now}
	if err := s.store.Update(func(state *State) error {
		state.Projects = append(state.Projects, project)
		return nil
	}); err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, project)
}

func (s *HTTPServer) updateProject(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var updated Project
	err := s.store.Update(func(state *State) error {
		project, err := findProject(state, r.PathValue("id"))
		if err != nil {
			return err
		}
		if input.Name != nil {
			name, err := normalizeName(*input.Name, 100)
			if err != nil {
				return fmt.Errorf("name %w", err)
			}
			project.Name = name
		}
		if input.Description != nil {
			if len([]rune(*input.Description)) > 1000 {
				return errors.New("description exceeds 1000 characters")
			}
			project.Description = strings.TrimSpace(*input.Description)
		}
		project.UpdatedAt = time.Now().UTC()
		updated = *project
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *HTTPServer) deleteProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.store.Update(func(state *State) error {
		if _, err := findProject(state, id); err != nil {
			return err
		}
		for _, task := range state.Tasks {
			if task.ProjectID == id && (task.Status == TaskQueued || task.Status == TaskRunning) {
				return errors.New("project has active tasks")
			}
		}
		state.Projects = deleteBy(state.Projects, func(item Project) bool { return item.ID == id })
		state.Scripts = deleteBy(state.Scripts, func(item Script) bool { return item.ProjectID == id })
		state.Shots = deleteBy(state.Shots, func(item Shot) bool { return item.ProjectID == id })
		state.Assets = deleteBy(state.Assets, func(item Asset) bool { return item.ProjectID == id })
		state.AssetLinks = deleteBy(state.AssetLinks, func(item AssetLink) bool { return item.ProjectID == id })
		state.Videos = deleteBy(state.Videos, func(item Video) bool { return item.ProjectID == id })
		state.Tasks = deleteBy(state.Tasks, func(item Task) bool { return item.ProjectID == id })
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if err := os.RemoveAll(filepath.Join(s.store.MediaDir(), id)); err != nil {
		s.logger.Warn("remove project media", "project_id", id, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *HTTPServer) listScripts(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	var scripts []Script
	_ = s.store.View(func(state State) error {
		for _, script := range state.Scripts {
			if projectID == "" || script.ProjectID == projectID {
				scripts = append(scripts, script)
			}
		}
		slices.SortFunc(scripts, func(a, b Script) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
		return nil
	})
	writeJSON(w, http.StatusOK, scripts)
}

func (s *HTTPServer) createScript(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProjectID string `json:"project_id"`
		Title     string `json:"title"`
		Content   string `json:"content"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	title, err := normalizeName(input.Title, 200)
	if err != nil {
		writeError(w, http.StatusBadRequest, "title "+err.Error())
		return
	}
	if len([]rune(input.Content)) > 500000 {
		writeError(w, http.StatusBadRequest, "content exceeds 500000 characters")
		return
	}
	id, err := newID("scr")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate script id")
		return
	}
	now := time.Now().UTC()
	script := Script{ID: id, ProjectID: input.ProjectID, Title: title, Content: input.Content, CreatedAt: now, UpdatedAt: now}
	if err := s.store.Update(func(state *State) error {
		if _, err := findProject(state, input.ProjectID); err != nil {
			return err
		}
		state.Scripts = append(state.Scripts, script)
		return nil
	}); err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, script)
}

func (s *HTTPServer) updateScript(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Title   *string `json:"title"`
		Content *string `json:"content"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var updated Script
	err := s.store.Update(func(state *State) error {
		for i := range state.Scripts {
			script := &state.Scripts[i]
			if script.ID != r.PathValue("id") {
				continue
			}
			if input.Title != nil {
				title, err := normalizeName(*input.Title, 200)
				if err != nil {
					return fmt.Errorf("title %w", err)
				}
				script.Title = title
			}
			if input.Content != nil {
				if len([]rune(*input.Content)) > 500000 {
					return errors.New("content exceeds 500000 characters")
				}
				script.Content = *input.Content
			}
			script.UpdatedAt = time.Now().UTC()
			updated = *script
			return nil
		}
		return ErrNotFound
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *HTTPServer) deleteScript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.store.Update(func(state *State) error {
		before := len(state.Scripts)
		state.Scripts = deleteBy(state.Scripts, func(item Script) bool { return item.ID == id })
		if len(state.Scripts) == before {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *HTTPServer) listShots(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	episodeID := r.URL.Query().Get("episode_id")
	status := r.URL.Query().Get("status")
	route := r.URL.Query().Get("route")
	var shots []ShotWithAssets
	_ = s.store.View(func(state State) error {
		for _, shot := range state.Shots {
			if (projectID != "" && shot.ProjectID != projectID) ||
				(episodeID != "" && shot.EpisodeID != episodeID) ||
				(status != "" && string(shot.ReviewStatus) != status) ||
				(route != "" && shot.GenerationRoute != route) {
				continue
			}
			item := ShotWithAssets{Shot: shot}
			for _, link := range state.AssetLinks {
				if link.TargetType == "shot" && link.TargetID == shot.ID {
					item.AssetLinks = append(item.AssetLinks, link)
				}
			}
			shots = append(shots, item)
		}
		slices.SortFunc(shots, func(a, b ShotWithAssets) int { return strings.Compare(a.ID, b.ID) })
		return nil
	})
	writeJSON(w, http.StatusOK, shots)
}

func (s *HTTPServer) importShots(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProjectID       string `json:"project_id"`
		ReplaceExisting bool   `json:"replace_existing"`
		Shots           []Shot `json:"shots"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(input.Shots) == 0 || len(input.Shots) > 1000 {
		writeError(w, http.StatusBadRequest, "shots must contain 1-1000 items")
		return
	}
	seen := make(map[string]bool, len(input.Shots))
	for i := range input.Shots {
		shot := &input.Shots[i]
		shot.ProjectID = input.ProjectID
		shot.ID = strings.TrimSpace(shot.ID)
		shot.EpisodeID = strings.TrimSpace(shot.EpisodeID)
		shot.SceneID = strings.TrimSpace(shot.SceneID)
		shot.ChapterTitle = strings.TrimSpace(shot.ChapterTitle)
		shot.Framing = strings.TrimSpace(shot.Framing)
		shot.Camera = strings.TrimSpace(shot.Camera)
		shot.Visual = strings.TrimSpace(shot.Visual)
		shot.Audio = strings.TrimSpace(shot.Audio)
		shot.SourceMode = strings.TrimSpace(shot.SourceMode)
		shot.AspectRatio = strings.TrimSpace(shot.AspectRatio)
		shot.TargetModel = strings.TrimSpace(shot.TargetModel)
		shot.GenerationRoute = strings.TrimSpace(shot.GenerationRoute)
		shot.Prompt = strings.TrimSpace(shot.Prompt)
		shot.NegativePrompt = strings.TrimSpace(shot.NegativePrompt)
		shot.InputVersion = strings.TrimSpace(shot.InputVersion)
		if err := validateImportedShot(*shot); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("shot %d: %v", i+1, err))
			return
		}
		if seen[shot.ID] {
			writeError(w, http.StatusBadRequest, "duplicate shot_id in import: "+shot.ID)
			return
		}
		seen[shot.ID] = true
	}
	var imported, updated, skipped int
	err := s.store.Update(func(state *State) error {
		if _, err := findProject(state, input.ProjectID); err != nil {
			return err
		}
		existing := make(map[string]int, len(state.Shots))
		for i, shot := range state.Shots {
			existing[shot.ID] = i
		}
		now := time.Now().UTC()
		for _, shot := range input.Shots {
			if index, ok := existing[shot.ID]; ok {
				if !input.ReplaceExisting {
					skipped++
					continue
				}
				current := state.Shots[index]
				shot.ReviewStatus = ShotPending
				shot.ReviewNote = ""
				shot.TaskID = current.TaskID
				shot.VideoID = current.VideoID
				shot.CreatedAt = current.CreatedAt
				shot.UpdatedAt = now
				state.Shots[index] = shot
				updated++
				continue
			}
			shot.ReviewStatus = ShotPending
			shot.ReviewNote = ""
			shot.TaskID = ""
			shot.VideoID = ""
			shot.CreatedAt = now
			shot.UpdatedAt = now
			for i := range state.Videos {
				video := &state.Videos[i]
				if video.ProjectID == input.ProjectID && strings.EqualFold(strings.TrimSuffix(video.Filename, filepath.Ext(video.Filename)), shot.ID) {
					shot.VideoID = video.ID
					video.ShotID = shot.ID
					break
				}
			}
			state.Shots = append(state.Shots, shot)
			existing[shot.ID] = len(state.Shots) - 1
			imported++
		}
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int{"imported": imported, "updated": updated, "skipped": skipped})
}

func validateImportedShot(shot Shot) error {
	if !shotIDPattern.MatchString(shot.ID) {
		return errors.New("invalid shot_id")
	}
	if shot.EpisodeID == "" || shot.SceneID == "" || !strings.HasPrefix(shot.SceneID, shot.EpisodeID+"-") || !strings.HasPrefix(shot.ID, shot.SceneID+"-") {
		return errors.New("episode_id and scene_id must match shot_id")
	}
	if shot.Chapter < 1 || shot.ChapterTitle == "" || shot.Visual == "" || shot.Prompt == "" {
		return errors.New("chapter, chapter_title, visual, and prompt are required")
	}
	if len([]rune(shot.Prompt)) > 7000 || len([]rune(shot.NegativePrompt)) > 3000 {
		return errors.New("prompt exceeds 7000 or negative_prompt exceeds 3000 characters")
	}
	if !slices.Contains([]string{"video_api", "post_production"}, shot.GenerationRoute) {
		return errors.New("generation_route must be video_api or post_production")
	}
	return validateVideoOptions(shot.TargetModel, shot.Duration, "768P", shot.AspectRatio)
}

func (s *HTTPServer) reviewShot(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Status ShotReviewStatus `json:"status"`
		Note   string           `json:"note"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !slices.Contains([]ShotReviewStatus{ShotPending, ShotApproved, ShotChangesRequested, ShotRejected}, input.Status) {
		writeError(w, http.StatusBadRequest, "invalid review status")
		return
	}
	input.Note = strings.TrimSpace(input.Note)
	if len([]rune(input.Note)) > 2000 {
		writeError(w, http.StatusBadRequest, "note exceeds 2000 characters")
		return
	}
	var updated Shot
	err := s.store.Update(func(state *State) error {
		shot, err := findShot(state, r.PathValue("id"))
		if err != nil {
			return err
		}
		shot.ReviewStatus = input.Status
		shot.ReviewNote = input.Note
		shot.UpdatedAt = time.Now().UTC()
		updated = *shot
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *HTTPServer) generateShot(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Provider             string   `json:"provider"`
		Resolution           string   `json:"resolution"`
		UseFrameImages       bool     `json:"use_frame_images"`
		CharacterPromptCount int      `json:"character_prompt_count"`
		ReferenceImageIDs    []string `json:"reference_image_ids"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	input.Provider = strings.ToLower(strings.TrimSpace(input.Provider))
	if input.Provider == "" {
		input.Provider = "mock"
	}
	if input.Resolution == "" {
		input.Resolution = "768P"
	}
	if !slices.Contains([]string{"mock", "minimax"}, input.Provider) {
		writeError(w, http.StatusBadRequest, "provider must be mock or minimax")
		return
	}
	if input.CharacterPromptCount < 0 || input.CharacterPromptCount > maxCharacterPromptModels {
		writeError(w, http.StatusBadRequest, "character_prompt_count must be 0-3")
		return
	}
	if input.UseFrameImages && len(input.ReferenceImageIDs) > 0 {
		writeError(w, http.StatusBadRequest, "frame images and reference images cannot be combined")
		return
	}
	if len(input.ReferenceImageIDs) > maxReferenceImages {
		writeError(w, http.StatusBadRequest, "reference_image_ids must contain at most 9 items")
		return
	}
	seenReferences := make(map[string]bool, len(input.ReferenceImageIDs))
	for i, id := range input.ReferenceImageIDs {
		id = strings.TrimSpace(id)
		if id == "" || seenReferences[id] {
			writeError(w, http.StatusBadRequest, "reference_image_ids must contain unique non-empty ids")
			return
		}
		seenReferences[id] = true
		input.ReferenceImageIDs[i] = id
	}
	if input.Provider == "minimax" && (!s.paidAllowed || !s.miniMaxConfigured) {
		writeError(w, http.StatusConflict, "MiniMax requires MINIMAX_API_KEY and ALLOW_PAID_GENERATION=true")
		return
	}
	var task Task
	err := s.store.Update(func(state *State) error {
		shot, err := findShot(state, r.PathValue("id"))
		if err != nil {
			return err
		}
		if shot.ReviewStatus != ShotApproved {
			return errors.New("shot must be approved before generation")
		}
		if shot.GenerationRoute != "video_api" {
			return errors.New("shot generation_route must be video_api")
		}
		if shot.RequiresEditorialSplit {
			return errors.New("shot requires editorial split before generation")
		}
		for _, existing := range state.Tasks {
			if existing.ShotID == shot.ID && (existing.Status == TaskQueued || existing.Status == TaskRunning) {
				return errors.New("shot already has an active task")
			}
		}
		if err := validateVideoOptions(shot.TargetModel, shot.Duration, input.Resolution, shot.AspectRatio); err != nil {
			return err
		}
		id, err := newID("tsk")
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		task = Task{
			ID: id, ProjectID: shot.ProjectID, ShotID: shot.ID, Kind: "video_generation",
			Provider: input.Provider, Model: shot.TargetModel, Prompt: shot.Prompt,
			Duration: shot.Duration, Resolution: input.Resolution, AspectRatio: shot.AspectRatio,
			Status: TaskQueued, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now,
			UseFrameImages: input.UseFrameImages, CharacterPromptCount: input.CharacterPromptCount,
			ReferenceImageIDs: slices.Clone(input.ReferenceImageIDs),
			InputVersion:      shot.InputVersion,
		}
		appendTaskLog(&task, "镜头 "+shot.ID+" 生成任务已创建")
		state.Tasks = append(state.Tasks, task)
		shot.TaskID = task.ID
		shot.UpdatedAt = now
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if err := s.worker.Enqueue(task.ID); err != nil {
		_ = s.store.Update(func(state *State) error {
			stored, findErr := findTask(state, task.ID)
			if findErr == nil {
				stored.Status = TaskFailed
				stored.Error = err.Error()
			}
			return findErr
		})
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

func (s *HTTPServer) generateShotSequence(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProjectID            string            `json:"project_id"`
		ShotIDs              []string          `json:"shot_ids"`
		PreviousTaskID       string            `json:"previous_task_id"`
		PromptOverrides      map[string]string `json:"prompt_overrides"`
		Provider             string            `json:"provider"`
		Resolution           string            `json:"resolution"`
		CharacterPromptCount int               `json:"character_prompt_count"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.PreviousTaskID = strings.TrimSpace(input.PreviousTaskID)
	input.Provider = strings.ToLower(strings.TrimSpace(input.Provider))
	if input.Resolution == "" {
		input.Resolution = "768P"
	}
	if input.ProjectID == "" || len(input.ShotIDs) < 2 || len(input.ShotIDs) > 20 || input.Provider != "minimax" {
		writeError(w, http.StatusBadRequest, "project_id, 2-20 shot_ids, and provider=minimax are required")
		return
	}
	if input.CharacterPromptCount < 0 || input.CharacterPromptCount > maxCharacterPromptModels {
		writeError(w, http.StatusBadRequest, "character_prompt_count must be 0-3")
		return
	}
	if !s.paidAllowed || !s.miniMaxConfigured {
		writeError(w, http.StatusConflict, "MiniMax requires MINIMAX_API_KEY and ALLOW_PAID_GENERATION=true")
		return
	}
	seen := make(map[string]bool, len(input.ShotIDs))
	for i, id := range input.ShotIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			writeError(w, http.StatusBadRequest, "shot_ids must be unique non-empty ids")
			return
		}
		seen[id] = true
		input.ShotIDs[i] = id
	}
	for shotID, prompt := range input.PromptOverrides {
		if !seen[shotID] || strings.TrimSpace(prompt) == "" || len([]rune(prompt)) > 7000 {
			writeError(w, http.StatusBadRequest, "prompt_overrides must target selected shots with 1-7000 characters")
			return
		}
		input.PromptOverrides[shotID] = strings.TrimSpace(prompt)
	}
	tasks := []Task{}
	err := s.store.Update(func(state *State) error {
		if _, err := findProject(state, input.ProjectID); err != nil {
			return err
		}
		previousTaskID := input.PreviousTaskID
		if previousTaskID != "" {
			previous, err := findTask(state, previousTaskID)
			if err != nil || previous.ProjectID != input.ProjectID || previous.Status != TaskSucceeded || previous.VideoID == "" {
				return errors.New("previous_task_id must be a succeeded project video task")
			}
		}
		for _, shotID := range input.ShotIDs {
			shot, err := findShot(state, shotID)
			if err != nil {
				return err
			}
			if shot.ProjectID != input.ProjectID || shot.ReviewStatus != ShotApproved || shot.GenerationRoute != "video_api" || shot.RequiresEditorialSplit {
				return errors.New("all sequence shots must pass the generation gate")
			}
			for _, task := range state.Tasks {
				if task.ShotID == shot.ID && (task.Status == TaskQueued || task.Status == TaskRunning) {
					return fmt.Errorf("shot %s already has an active task", shot.ID)
				}
			}
			if err := validateVideoOptions(shot.TargetModel, shot.Duration, input.Resolution, shot.AspectRatio); err != nil {
				return err
			}
			id, err := newID("tsk")
			if err != nil {
				return err
			}
			now := time.Now().UTC()
			prompt := shot.Prompt
			if override, ok := input.PromptOverrides[shot.ID]; ok {
				prompt = override
			}
			task := Task{
				ID: id, ProjectID: shot.ProjectID, ShotID: shot.ID, Kind: "video_generation",
				Provider: input.Provider, Model: shot.TargetModel, Prompt: prompt,
				Duration: shot.Duration, Resolution: input.Resolution, AspectRatio: shot.AspectRatio,
				Status: TaskQueued, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now,
				CharacterPromptCount: input.CharacterPromptCount, PreviousTaskID: previousTaskID,
				InputVersion: shot.InputVersion,
			}
			appendTaskLog(&task, "顺序试片任务已创建")
			state.Tasks = append(state.Tasks, task)
			shot.TaskID = task.ID
			shot.UpdatedAt = now
			tasks = append(tasks, task)
			previousTaskID = task.ID
		}
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if err := s.worker.Enqueue(tasks[0].ID); err != nil {
		s.worker.fail(tasks[0].ID, err)
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, tasks)
}

func (s *HTTPServer) generateShotImages(w http.ResponseWriter, r *http.Request) {
	if !s.openAIConfigured {
		writeError(w, http.StatusConflict, "GPT Image 2 requires OPENAI_API_KEY")
		return
	}
	var input struct {
		ProjectID string `json:"project_id"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	if input.ProjectID == "" {
		writeError(w, http.StatusBadRequest, "project_id is required")
		return
	}
	var queued []Task
	var skipped int
	err := s.store.Update(func(state *State) error {
		if _, err := findProject(state, input.ProjectID); err != nil {
			return err
		}
		shots := make([]Shot, 0)
		for _, shot := range state.Shots {
			if shot.ProjectID == input.ProjectID {
				shots = append(shots, shot)
			}
		}
		slices.SortFunc(shots, func(a, b Shot) int { return strings.Compare(a.ID, b.ID) })
		for _, shot := range shots {
			tasks, omitted, err := newShotImageTasks(state, shot)
			if err != nil {
				return err
			}
			queued = append(queued, tasks...)
			skipped += omitted
			state.Tasks = append(state.Tasks, tasks...)
		}
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	for _, task := range queued {
		if err := s.worker.Enqueue(task.ID); err != nil {
			s.worker.fail(task.ID, err)
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"queued": len(queued), "skipped": skipped})
}

func (s *HTTPServer) generateShotImageSet(w http.ResponseWriter, r *http.Request) {
	if !s.openAIConfigured {
		writeError(w, http.StatusConflict, "GPT Image 2 requires OPENAI_API_KEY")
		return
	}

	var queued []Task
	var skipped int
	err := s.store.Update(func(state *State) error {
		shot, err := findShot(state, r.PathValue("id"))
		if err != nil {
			return err
		}
		queued, skipped, err = newShotImageTasks(state, *shot)
		if err != nil {
			return err
		}
		state.Tasks = append(state.Tasks, queued...)
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	for _, task := range queued {
		if err := s.worker.Enqueue(task.ID); err != nil {
			s.worker.fail(task.ID, err)
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"queued": len(queued), "skipped": skipped})
}

func (s *HTTPServer) generateCharacterImages(w http.ResponseWriter, r *http.Request) {
	if !s.openAIConfigured {
		writeError(w, http.StatusConflict, "GPT Image 2 requires OPENAI_API_KEY")
		return
	}
	var input struct {
		ProjectID string `json:"project_id"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	if input.ProjectID == "" {
		writeError(w, http.StatusBadRequest, "project_id is required")
		return
	}

	var characters []Asset
	if err := s.store.View(func(state State) error {
		if _, err := findProject(&state, input.ProjectID); err != nil {
			return err
		}
		for _, asset := range state.Assets {
			if asset.ProjectID == input.ProjectID && asset.Kind == "character" {
				characters = append(characters, asset)
			}
		}
		return nil
	}); err != nil {
		s.writeStoreError(w, err)
		return
	}
	slices.SortFunc(characters, func(a, b Asset) int { return strings.Compare(a.Name, b.Name) })

	models := make(map[string]string, len(characters))
	for _, character := range characters {
		path, err := s.mediaPath(character.StoragePath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "invalid stored character model path")
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			s.logger.Error("read character model", "asset_id", character.ID, "error", err)
			writeError(w, http.StatusInternalServerError, "read character model failed")
			return
		}
		if len(data) == 0 || len(data) > maxTextPreviewBytes {
			writeError(w, http.StatusConflict, "character model must contain 1 byte to 1 MiB")
			return
		}
		var model map[string]any
		if json.Unmarshal(data, &model) != nil || len(model) == 0 {
			writeError(w, http.StatusConflict, "character model must be a non-empty JSON object")
			return
		}
		models[character.ID] = string(data)
	}

	var queued []Task
	var skipped int
	err := s.store.Update(func(state *State) error {
		if _, err := findProject(state, input.ProjectID); err != nil {
			return err
		}
		for _, character := range characters {
			if _, err := findAsset(state, character.ID); err != nil {
				return err
			}
			tasks, omitted, err := newCharacterImageTasks(state, character, models[character.ID])
			if err != nil {
				return err
			}
			queued = append(queued, tasks...)
			skipped += omitted
			state.Tasks = append(state.Tasks, tasks...)
		}
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	for _, task := range queued {
		if err := s.worker.Enqueue(task.ID); err != nil {
			s.worker.fail(task.ID, err)
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]int{
		"characters": len(characters),
		"queued":     len(queued),
		"skipped":    skipped,
	})
}

func (s *HTTPServer) linkShotAsset(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AssetID string `json:"asset_id"`
		Note    string `json:"note"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	input.AssetID = strings.TrimSpace(input.AssetID)
	input.Note = strings.TrimSpace(input.Note)
	if input.AssetID == "" || len([]rune(input.Note)) > 500 {
		writeError(w, http.StatusBadRequest, "asset_id is required and note must not exceed 500 characters")
		return
	}
	var created AssetLink
	err := s.store.Update(func(state *State) error {
		shot, err := findShot(state, r.PathValue("id"))
		if err != nil {
			return err
		}
		var asset *Asset
		for i := range state.Assets {
			if state.Assets[i].ID == input.AssetID && state.Assets[i].ProjectID == shot.ProjectID {
				asset = &state.Assets[i]
				break
			}
		}
		if asset == nil {
			return errors.New("resource not found in this project")
		}
		for _, link := range state.AssetLinks {
			if link.AssetID == asset.ID && link.TargetType == "shot" && link.TargetID == shot.ID {
				return errors.New("resource link already exists")
			}
		}
		id, err := newID("lnk")
		if err != nil {
			return err
		}
		created = AssetLink{ID: id, ProjectID: shot.ProjectID, AssetID: asset.ID, TargetType: "shot", TargetID: shot.ID, Note: input.Note, CreatedAt: time.Now().UTC()}
		state.AssetLinks = append(state.AssetLinks, created)
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *HTTPServer) unlinkShotAsset(w http.ResponseWriter, r *http.Request) {
	err := s.store.Update(func(state *State) error {
		if _, err := findShot(state, r.PathValue("id")); err != nil {
			return err
		}
		linkID := r.PathValue("linkID")
		before := len(state.AssetLinks)
		state.AssetLinks = deleteBy(state.AssetLinks, func(link AssetLink) bool {
			return link.ID == linkID && link.TargetType == "shot" && link.TargetID == r.PathValue("id")
		})
		if len(state.AssetLinks) == before {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *HTTPServer) listAssets(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	var assets []AssetWithLinks
	_ = s.store.View(func(state State) error {
		for _, asset := range state.Assets {
			if projectID == "" || asset.ProjectID == projectID {
				item := AssetWithLinks{Asset: asset}
				for _, link := range state.AssetLinks {
					if link.AssetID == asset.ID {
						item.Links = append(item.Links, link)
					}
				}
				assets = append(assets, item)
			}
		}
		slices.SortFunc(assets, func(a, b AssetWithLinks) int { return b.CreatedAt.Compare(a.CreatedAt) })
		return nil
	})
	writeJSON(w, http.StatusOK, assets)
}

func (s *HTTPServer) uploadAsset(w http.ResponseWriter, r *http.Request) {
	s.upload(w, r, false)
}

func (s *HTTPServer) assetContent(w http.ResponseWriter, r *http.Request) {
	var asset Asset
	err := s.store.View(func(state State) error {
		for _, item := range state.Assets {
			if item.ID == r.PathValue("id") {
				asset = item
				return nil
			}
		}
		return ErrNotFound
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.serveMedia(w, r, asset.StoragePath, asset.Filename, asset.ContentType)
}

func (s *HTTPServer) assetPreview(w http.ResponseWriter, r *http.Request) {
	var asset Asset
	err := s.store.View(func(state State) error {
		for _, item := range state.Assets {
			if item.ID == r.PathValue("id") {
				asset = item
				return nil
			}
		}
		return ErrNotFound
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if !isTextAsset(asset) {
		writeError(w, http.StatusUnsupportedMediaType, "preview is only available for text and JSON resources")
		return
	}
	path, err := s.mediaPath(asset.StoragePath)
	if err != nil {
		s.logger.Error("resolve asset preview", "asset_id", asset.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "stored media not found")
			return
		}
		s.logger.Error("read asset preview", "asset_id", asset.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if len(data) > maxTextPreviewBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "preview exceeds 1 MiB")
		return
	}
	content := string(data)
	if strings.HasSuffix(strings.ToLower(asset.Filename), ".json") {
		var value any
		if json.Unmarshal(data, &value) == nil {
			formatted, err := json.MarshalIndent(value, "", "  ")
			if err != nil {
				s.logger.Error("format JSON preview", "asset_id", asset.ID, "error", err)
				writeError(w, http.StatusInternalServerError, "internal server error")
				return
			}
			content = string(formatted)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"content": content})
}
func (s *HTTPServer) createAssetLink(w http.ResponseWriter, r *http.Request) {
	var input struct {
		TargetType string `json:"target_type"`
		TargetID   string `json:"target_id"`
		Note       string `json:"note"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	input.TargetType = strings.TrimSpace(input.TargetType)
	input.TargetID = strings.TrimSpace(input.TargetID)
	input.Note = strings.TrimSpace(input.Note)
	if !slices.Contains([]string{"asset", "video", "shot"}, input.TargetType) || input.TargetID == "" {
		writeError(w, http.StatusBadRequest, "target_type must be asset, video, or shot and target_id is required")
		return
	}
	if len([]rune(input.Note)) > 500 {
		writeError(w, http.StatusBadRequest, "note exceeds 500 characters")
		return
	}
	var created AssetLink
	err := s.store.Update(func(state *State) error {
		var source Asset
		found := false
		for _, asset := range state.Assets {
			if asset.ID == r.PathValue("id") {
				source = asset
				found = true
				break
			}
		}
		if !found {
			return ErrNotFound
		}
		targetOK := false
		if input.TargetType == "asset" {
			for _, target := range state.Assets {
				if target.ID == input.TargetID && target.ProjectID == source.ProjectID {
					targetOK = true
					break
				}
			}
		} else if input.TargetType == "video" {
			for _, target := range state.Videos {
				if target.ID == input.TargetID && target.ProjectID == source.ProjectID {
					targetOK = true
					break
				}
			}
		} else {
			for _, target := range state.Shots {
				if target.ID == input.TargetID && target.ProjectID == source.ProjectID {
					targetOK = true
					break
				}
			}
		}
		if !targetOK {
			return errors.New("target not found in this project")
		}
		for _, link := range state.AssetLinks {
			if link.AssetID == source.ID && link.TargetType == input.TargetType && link.TargetID == input.TargetID {
				return errors.New("resource link already exists")
			}
		}
		id, err := newID("lnk")
		if err != nil {
			return err
		}
		created = AssetLink{
			ID: id, ProjectID: source.ProjectID, AssetID: source.ID,
			TargetType: input.TargetType, TargetID: input.TargetID, Note: input.Note, CreatedAt: time.Now().UTC(),
		}
		state.AssetLinks = append(state.AssetLinks, created)
		return nil
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *HTTPServer) deleteAssetLink(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	linkID := r.PathValue("linkID")
	err := s.store.Update(func(state *State) error {
		for _, link := range state.AssetLinks {
			if link.ID == linkID && link.AssetID == assetID {
				state.AssetLinks = deleteBy(state.AssetLinks, func(item AssetLink) bool { return item.ID == linkID })
				return nil
			}
		}
		return ErrNotFound
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *HTTPServer) deleteAsset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var storagePath string
	err := s.store.Update(func(state *State) error {
		for _, item := range state.Assets {
			if item.ID == id {
				storagePath = item.StoragePath
				state.Assets = deleteBy(state.Assets, func(asset Asset) bool { return asset.ID == id })
				state.AssetLinks = deleteBy(state.AssetLinks, func(link AssetLink) bool {
					return link.AssetID == id || (link.TargetType == "asset" && link.TargetID == id)
				})
				return nil
			}
		}
		return ErrNotFound
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.removeMedia(storagePath)
	w.WriteHeader(http.StatusNoContent)
}

func (s *HTTPServer) listVideos(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	var videos []Video
	_ = s.store.View(func(state State) error {
		for _, video := range state.Videos {
			if projectID == "" || video.ProjectID == projectID {
				videos = append(videos, video)
			}
		}
		slices.SortFunc(videos, func(a, b Video) int {
			if a.ShotID != "" && b.ShotID != "" && a.ShotID != b.ShotID {
				return strings.Compare(a.ShotID, b.ShotID)
			}
			if a.ShotID != b.ShotID {
				if a.ShotID == "" {
					return 1
				}
				return -1
			}
			return b.CreatedAt.Compare(a.CreatedAt)
		})
		return nil
	})
	writeJSON(w, http.StatusOK, videos)
}

func (s *HTTPServer) reviewVideo(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Status VideoReviewStatus `json:"status"`
		Note   string            `json:"note"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !slices.Contains([]VideoReviewStatus{VideoUnreviewed, VideoUsable, VideoRejected}, input.Status) {
		writeError(w, http.StatusBadRequest, "invalid video review status")
		return
	}
	input.Note = strings.TrimSpace(input.Note)
	if len([]rune(input.Note)) > 1000 {
		writeError(w, http.StatusBadRequest, "note exceeds 1000 characters")
		return
	}
	var updated Video
	err := s.store.Update(func(state *State) error {
		for i := range state.Videos {
			video := &state.Videos[i]
			if video.ID != r.PathValue("id") {
				continue
			}
			now := time.Now().UTC()
			video.ReviewStatus = input.Status
			video.ReviewNote = input.Note
			video.ReviewUpdatedAt = &now
			updated = *video
			return nil
		}
		return ErrNotFound
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *HTTPServer) uploadVideo(w http.ResponseWriter, r *http.Request) {
	s.upload(w, r, true)
}

func (s *HTTPServer) videoContent(w http.ResponseWriter, r *http.Request) {
	var video Video
	err := s.store.View(func(state State) error {
		for _, item := range state.Videos {
			if item.ID == r.PathValue("id") {
				video = item
				return nil
			}
		}
		return ErrNotFound
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.serveMedia(w, r, video.StoragePath, video.Filename, video.ContentType)
}

func (s *HTTPServer) deleteVideo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var storagePath string
	err := s.store.Update(func(state *State) error {
		for _, item := range state.Videos {
			if item.ID == id {
				storagePath = item.StoragePath
				state.Videos = deleteBy(state.Videos, func(video Video) bool { return video.ID == id })
				state.AssetLinks = deleteBy(state.AssetLinks, func(link AssetLink) bool { return link.TargetType == "video" && link.TargetID == id })
				for i := range state.Shots {
					if state.Shots[i].VideoID == id {
						state.Shots[i].VideoID = ""
						state.Shots[i].UpdatedAt = time.Now().UTC()
					}
				}
				for i := range state.Tasks {
					task := &state.Tasks[i]
					if task.VideoID == id {
						task.VideoID = ""
						task.UpdatedAt = time.Now().UTC()
						appendTaskLog(task, "关联试片已删除")
					}
				}
				return nil
			}
		}
		return ErrNotFound
	})
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.removeMedia(storagePath)
	w.WriteHeader(http.StatusNoContent)
}

func (s *HTTPServer) upload(w http.ResponseWriter, r *http.Request, video bool) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadBytes+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart upload")
		return
	}
	projectID := strings.TrimSpace(r.FormValue("project_id"))
	name, err := normalizeName(r.FormValue("name"), 200)
	if err != nil {
		writeError(w, http.StatusBadRequest, "name "+err.Error())
		return
	}
	if err := s.store.View(func(state State) error {
		for _, project := range state.Projects {
			if project.ID == projectID {
				return nil
			}
		}
		return ErrNotFound
	}); err != nil {
		s.writeStoreError(w, err)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "file is required")
		return
	}
	defer file.Close()
	prefix := "ast"
	directory := "assets"
	if video {
		prefix = "vid"
		directory = "videos"
	}
	id, err := newID(prefix)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate media id")
		return
	}
	filename := safeFilename(header.Filename)
	if filename == "" {
		writeError(w, http.StatusBadRequest, "invalid filename")
		return
	}
	relPath, size, contentType, err := s.saveUpload(projectID, directory, id, filename, file, header)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now().UTC()
	if video {
		item := Video{
			ID: id, ProjectID: projectID, Title: name, Filename: filename,
			ContentType: contentType, Size: size, StoragePath: relPath, Provider: "upload",
			ReviewStatus: VideoUnreviewed, CreatedAt: now,
		}
		if err := s.store.Update(func(state *State) error {
			for i := range state.Shots {
				shot := &state.Shots[i]
				if shot.ProjectID == projectID && strings.EqualFold(strings.TrimSuffix(filename, filepath.Ext(filename)), shot.ID) {
					item.ShotID = shot.ID
					shot.VideoID = item.ID
					shot.UpdatedAt = now
					break
				}
			}
			state.Videos = append(state.Videos, item)
			return nil
		}); err != nil {
			s.removeMedia(relPath)
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, item)
		return
	}
	kind := strings.TrimSpace(r.FormValue("kind"))
	if kind == "" {
		kind = "other"
	}
	if !slices.Contains([]string{"character", "scene", "prop", "costume", "image", "audio", "document", "other"}, kind) {
		s.removeMedia(relPath)
		writeError(w, http.StatusBadRequest, "invalid asset kind")
		return
	}
	item := Asset{
		ID: id, ProjectID: projectID, Name: name, Kind: kind, Filename: filename,
		ContentType: contentType, Size: size, StoragePath: relPath, CreatedAt: now,
	}
	if err := s.store.Update(func(state *State) error { state.Assets = append(state.Assets, item); return nil }); err != nil {
		s.removeMedia(relPath)
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *HTTPServer) saveUpload(
	projectID, directory, id, filename string,
	file multipart.File,
	header *multipart.FileHeader,
) (string, int64, string, error) {
	dir := filepath.Join(s.store.MediaDir(), projectID, directory, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", 0, "", fmt.Errorf("create upload directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return "", 0, "", fmt.Errorf("create upload file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	written, err := io.Copy(tmp, io.LimitReader(file, s.maxUploadBytes+1))
	if err != nil {
		_ = tmp.Close()
		return "", 0, "", fmt.Errorf("save upload: %w", err)
	}
	if written > s.maxUploadBytes {
		_ = tmp.Close()
		return "", 0, "", fmt.Errorf("upload exceeds %d bytes", s.maxUploadBytes)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", 0, "", fmt.Errorf("sync upload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, "", fmt.Errorf("close upload: %w", err)
	}
	finalPath := filepath.Join(dir, filename)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return "", 0, "", fmt.Errorf("store upload: %w", err)
	}
	relPath, err := filepath.Rel(s.store.MediaDir(), finalPath)
	if err != nil {
		return "", 0, "", fmt.Errorf("resolve upload path: %w", err)
	}
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return filepath.ToSlash(relPath), written, contentType, nil
}

func (s *HTTPServer) serveMedia(w http.ResponseWriter, r *http.Request, storagePath, filename, contentType string) {
	path, err := s.mediaPath(storagePath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "invalid stored media path")
		return
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Content-Disposition", "inline; filename="+strconv.Quote(filename))
	http.ServeFile(w, r, path)
}

func (s *HTTPServer) mediaPath(storagePath string) (string, error) {
	if !filepath.IsLocal(filepath.FromSlash(storagePath)) {
		return "", errors.New("non-local storage path")
	}
	path := filepath.Join(s.store.MediaDir(), filepath.FromSlash(storagePath))
	rel, err := filepath.Rel(s.store.MediaDir(), path)
	if err != nil || !filepath.IsLocal(rel) {
		return "", errors.New("storage path escapes media directory")
	}
	return path, nil
}

func isTextAsset(asset Asset) bool {
	if strings.HasPrefix(asset.ContentType, "text/") || asset.ContentType == "application/json" {
		return true
	}
	return slices.Contains([]string{".json", ".jsonl", ".md", ".txt", ".py", ".go", ".yaml", ".yml"}, strings.ToLower(filepath.Ext(asset.Filename)))
}

func (s *HTTPServer) removeMedia(storagePath string) {
	if !filepath.IsLocal(filepath.FromSlash(storagePath)) {
		return
	}
	path := filepath.Join(s.store.MediaDir(), filepath.FromSlash(storagePath))
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		s.logger.Warn("remove media", "path", storagePath, "error", err)
	}
}

func (s *HTTPServer) listTasks(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	var tasks []Task
	_ = s.store.View(func(state State) error {
		for _, task := range state.Tasks {
			if projectID == "" || task.ProjectID == projectID {
				tasks = append(tasks, task)
			}
		}
		slices.SortFunc(tasks, func(a, b Task) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
		return nil
	})
	writeJSON(w, http.StatusOK, tasks)
}

func (s *HTTPServer) createTask(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusConflict, "create generation tasks through an approved shot")
}

func (s *HTTPServer) cancelTask(w http.ResponseWriter, r *http.Request) {
	if err := s.worker.Cancel(r.PathValue("id")); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *HTTPServer) retryTask(w http.ResponseWriter, r *http.Request) {
	if err := s.worker.Retry(r.PathValue("id")); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func validateVideoOptions(model string, duration int, resolution, aspectRatio string) error {
	if model != "MiniMax-H3" {
		return errors.New("model must be MiniMax-H3")
	}
	if duration < 4 || duration > 15 {
		return errors.New("duration must be 4-15 seconds")
	}
	if !slices.Contains([]string{"768P", "2K"}, resolution) {
		return errors.New("resolution must be 768P or 2K")
	}
	if !slices.Contains([]string{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"}, aspectRatio) {
		return errors.New("aspect_ratio must be 21:9, 16:9, 4:3, 1:1, 3:4, or 9:16")
	}
	return nil
}

func (s *HTTPServer) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	default:
		message := err.Error()
		if strings.Contains(message, "must") || strings.Contains(message, "requires") || strings.Contains(message, "exceeds") || strings.Contains(message, "active task") || strings.Contains(message, "only failed") || strings.Contains(message, "retry limit") || strings.Contains(message, "already") || strings.Contains(message, "not found in this project") {
			writeError(w, http.StatusConflict, message)
			return
		}
		s.logger.Error("store operation", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func deleteBy[S ~[]E, E any](items S, remove func(E) bool) S {
	return slices.DeleteFunc(items, remove)
}
