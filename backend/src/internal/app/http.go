package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const maxJSONBody = 2 << 20

type HTTPServer struct {
	store             *Store
	worker            *Worker
	logger            *slog.Logger
	maxUploadBytes    int64
	paidAllowed       bool
	miniMaxConfigured bool
}

func NewHTTPServer(
	store *Store,
	worker *Worker,
	logger *slog.Logger,
	maxUploadBytes int64,
	paidAllowed bool,
	miniMaxConfigured bool,
) http.Handler {
	s := &HTTPServer{
		store: store, worker: worker, logger: logger, maxUploadBytes: maxUploadBytes,
		paidAllowed: paidAllowed, miniMaxConfigured: miniMaxConfigured,
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
	mux.HandleFunc("GET /api/assets", s.listAssets)
	mux.HandleFunc("POST /api/assets", s.uploadAsset)
	mux.HandleFunc("GET /api/assets/{id}/content", s.assetContent)
	mux.HandleFunc("DELETE /api/assets/{id}", s.deleteAsset)
	mux.HandleFunc("GET /api/videos", s.listVideos)
	mux.HandleFunc("POST /api/videos", s.uploadVideo)
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
		state.Assets = deleteBy(state.Assets, func(item Asset) bool { return item.ProjectID == id })
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

func (s *HTTPServer) listAssets(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	var assets []Asset
	_ = s.store.View(func(state State) error {
		for _, asset := range state.Assets {
			if projectID == "" || asset.ProjectID == projectID {
				assets = append(assets, asset)
			}
		}
		slices.SortFunc(assets, func(a, b Asset) int { return b.CreatedAt.Compare(a.CreatedAt) })
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

func (s *HTTPServer) deleteAsset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var storagePath string
	err := s.store.Update(func(state *State) error {
		for _, item := range state.Assets {
			if item.ID == id {
				storagePath = item.StoragePath
				state.Assets = deleteBy(state.Assets, func(asset Asset) bool { return asset.ID == id })
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
		slices.SortFunc(videos, func(a, b Video) int { return b.CreatedAt.Compare(a.CreatedAt) })
		return nil
	})
	writeJSON(w, http.StatusOK, videos)
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
			ContentType: contentType, Size: size, StoragePath: relPath, Provider: "upload", CreatedAt: now,
		}
		if err := s.store.Update(func(state *State) error { state.Videos = append(state.Videos, item); return nil }); err != nil {
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
	if !filepath.IsLocal(filepath.FromSlash(storagePath)) {
		writeError(w, http.StatusInternalServerError, "invalid stored media path")
		return
	}
	path := filepath.Join(s.store.MediaDir(), filepath.FromSlash(storagePath))
	rel, err := filepath.Rel(s.store.MediaDir(), path)
	if err != nil || !filepath.IsLocal(rel) {
		writeError(w, http.StatusInternalServerError, "invalid stored media path")
		return
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Content-Disposition", "inline; filename="+strconv.Quote(filename))
	http.ServeFile(w, r, path)
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
	var input struct {
		ProjectID       string `json:"project_id"`
		Provider        string `json:"provider"`
		Model           string `json:"model"`
		Prompt          string `json:"prompt"`
		FirstFrameImage string `json:"first_frame_image"`
		Duration        int    `json:"duration"`
		Resolution      string `json:"resolution"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	input.Provider = strings.ToLower(strings.TrimSpace(input.Provider))
	if input.Provider == "" {
		input.Provider = "mock"
	}
	if !slices.Contains([]string{"mock", "minimax"}, input.Provider) {
		writeError(w, http.StatusBadRequest, "provider must be mock or minimax")
		return
	}
	if input.Provider == "minimax" && (!s.paidAllowed || !s.miniMaxConfigured) {
		writeError(w, http.StatusConflict, "MiniMax requires MINIMAX_API_KEY and ALLOW_PAID_GENERATION=true")
		return
	}
	input.Prompt = strings.TrimSpace(input.Prompt)
	if input.Prompt == "" || len([]rune(input.Prompt)) > 2000 {
		writeError(w, http.StatusBadRequest, "prompt must contain 1-2000 characters")
		return
	}
	input.FirstFrameImage = strings.TrimSpace(input.FirstFrameImage)
	if input.FirstFrameImage != "" {
		u, err := url.Parse(input.FirstFrameImage)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(input.FirstFrameImage) > 2048 {
			writeError(w, http.StatusBadRequest, "first_frame_image must be a public HTTPS URL")
			return
		}
	}
	if err := validateVideoOptions(input.Model, input.FirstFrameImage != "", input.Duration, input.Resolution); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := newID("tsk")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate task id")
		return
	}
	now := time.Now().UTC()
	task := Task{
		ID: id, ProjectID: input.ProjectID, Kind: "video_generation", Provider: input.Provider,
		Model: input.Model, Prompt: input.Prompt, FirstFrameImage: input.FirstFrameImage,
		Duration: input.Duration, Resolution: input.Resolution, Status: TaskQueued,
		MaxAttempts: 3, CreatedAt: now, UpdatedAt: now,
	}
	appendTaskLog(&task, "任务已创建")
	if err := s.store.Update(func(state *State) error {
		if _, err := findProject(state, input.ProjectID); err != nil {
			return err
		}
		state.Tasks = append(state.Tasks, task)
		return nil
	}); err != nil {
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

func validateVideoOptions(model string, imageToVideo bool, duration int, resolution string) error {
	t2v := []string{"MiniMax-Hailuo-2.3", "MiniMax-Hailuo-02", "T2V-01-Director", "T2V-01"}
	i2v := []string{"MiniMax-Hailuo-2.3", "MiniMax-Hailuo-2.3-Fast", "MiniMax-Hailuo-02", "I2V-01-Director", "I2V-01-live", "I2V-01"}
	allowed := t2v
	if imageToVideo {
		allowed = i2v
	}
	if !slices.Contains(allowed, model) {
		return errors.New("model is not valid for selected generation mode")
	}
	if duration != 6 && duration != 10 {
		return errors.New("duration must be 6 or 10 seconds")
	}
	if !slices.Contains([]string{"512P", "720P", "768P", "1080P"}, resolution) {
		return errors.New("resolution must be 512P, 720P, 768P, or 1080P")
	}
	if duration == 10 && (resolution == "1080P" || (!strings.HasPrefix(model, "MiniMax-Hailuo") && resolution != "768P")) {
		return errors.New("selected model does not support this 10-second resolution")
	}
	return nil
}

func (s *HTTPServer) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	default:
		message := err.Error()
		if strings.Contains(message, "must") || strings.Contains(message, "exceeds") || strings.Contains(message, "active tasks") || strings.Contains(message, "only failed") || strings.Contains(message, "retry limit") || strings.Contains(message, "already") {
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
