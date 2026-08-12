package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	maxCharacterPromptModels = 3
	maxReferenceImages       = 9
	maxVideoInputBytes       = 45 << 20
	maxSingleInputImage      = 5 << 20
)

type Worker struct {
	store            *Store
	providers        map[string]VideoProvider
	imageProviders   map[string]ImageProvider
	queue            chan string
	pollInterval     time.Duration
	maxDownloadBytes int64
	downloadClient   *http.Client
	logger           *slog.Logger
	mu               sync.Mutex
	running          map[string]context.CancelFunc
	wg               sync.WaitGroup
	imageQueue       chan string
	imageWG          sync.WaitGroup
}

func NewWorker(
	store *Store,
	providers map[string]VideoProvider,
	pollInterval time.Duration,
	maxDownloadBytes int64,
	logger *slog.Logger,
) *Worker {
	return &Worker{
		store:            store,
		providers:        providers,
		imageProviders:   map[string]ImageProvider{},
		queue:            make(chan string, 4096),
		imageQueue:       make(chan string, 4096),
		pollInterval:     pollInterval,
		maxDownloadBytes: maxDownloadBytes,
		downloadClient:   &http.Client{Timeout: 20 * time.Minute},
		logger:           logger,
		running:          make(map[string]context.CancelFunc),
	}
}

func (w *Worker) SetImageProvider(name string, provider ImageProvider) {
	if w.imageProviders == nil {
		w.imageProviders = map[string]ImageProvider{}
	}
	w.imageProviders[name] = provider
}

func (w *Worker) Start(ctx context.Context, count int) error {
	if count < 1 {
		return errors.New("worker count must be positive")
	}
	var queued []string
	if err := w.store.Update(func(state *State) error {
		for i := range state.Tasks {
			task := &state.Tasks[i]
			if task.Status == TaskRunning {
				task.Status = TaskQueued
				task.UpdatedAt = time.Now().UTC()
				appendTaskLog(task, "服务重启，任务恢复排队")
			}
			if task.Status == TaskQueued {
				if task.PreviousTaskID != "" {
					previous, err := findTask(state, task.PreviousTaskID)
					if err != nil || previous.Status != TaskSucceeded || previous.VideoID == "" {
						continue
					}
				}
				if task.Kind == "image_generation" && !w.hasImageProvider(task.Provider) {
					appendTaskLog(task, "图片任务等待图片服务配置")
					continue
				}
				queued = append(queued, task.ID)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("recover tasks: %w", err)
	}
	for range count {
		w.wg.Add(1)
		go w.loop(ctx)
	}
	w.imageWG.Add(1)
	go w.imageLoop(ctx)
	for _, id := range queued {
		if err := w.enqueueTask(id); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) hasImageProvider(name string) bool {
	_, ok := w.imageProviders[name]
	return ok
}

func (w *Worker) Wait() {
	w.wg.Wait()
	w.imageWG.Wait()
}

func (w *Worker) Enqueue(taskID string) error {
	task, err := w.getTask(taskID)
	if err != nil {
		return err
	}
	if task.Kind == "image_generation" {
		return w.enqueueImage(taskID)
	}
	return w.enqueueVideo(taskID)
}

func (w *Worker) enqueueTask(taskID string) error {
	task, err := w.getTask(taskID)
	if err != nil {
		return err
	}
	if task.Kind == "image_generation" {
		return w.enqueueImage(taskID)
	}
	return w.enqueueVideo(taskID)
}

func (w *Worker) enqueueVideo(taskID string) error {
	select {
	case w.queue <- taskID:
		return nil
	default:
		return errors.New("task queue is full")
	}
}

func (w *Worker) enqueueImage(taskID string) error {
	select {
	case w.imageQueue <- taskID:
		return nil
	default:
		return errors.New("image task queue is full")
	}
}

func (w *Worker) Cancel(taskID string) error {
	if err := w.store.Update(func(state *State) error {
		task, err := findTask(state, taskID)
		if err != nil {
			return err
		}
		if task.Status == TaskSucceeded || task.Status == TaskFailed || task.Status == TaskCancelled {
			return fmt.Errorf("task is already %s", task.Status)
		}
		now := time.Now().UTC()
		task.Status = TaskCancelled
		task.UpdatedAt = now
		task.CompletedAt = &now
		appendTaskLog(task, "本地任务已取消；MiniMax H3 V2 远端任务不会被终止")
		return nil
	}); err != nil {
		return err
	}
	w.mu.Lock()
	cancel := w.running[taskID]
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (w *Worker) Retry(taskID string) error {
	if err := w.store.Update(func(state *State) error {
		task, err := findTask(state, taskID)
		if err != nil {
			return err
		}
		if task.Status != TaskFailed && task.Status != TaskCancelled {
			return errors.New("only failed or cancelled tasks can be retried")
		}
		if task.Kind == "image_generation" {
			if task.ShotID == "" && task.AssetID == "" {
				return errors.New("image task is not linked to a shot or asset")
			}
			if task.ShotID != "" {
				if _, err := findShot(state, task.ShotID); err != nil {
					return err
				}
			}
			if task.AssetID != "" {
				if _, err := findAsset(state, task.AssetID); err != nil {
					return err
				}
			}
		} else {
			if task.ShotID == "" {
				return errors.New("task is not linked to a shot")
			}
			shot, err := findShot(state, task.ShotID)
			if err != nil {
				return err
			}
			if shot.ReviewStatus != ShotApproved || shot.GenerationRoute != "video_api" || shot.RequiresEditorialSplit {
				return errors.New("shot must pass generation gate before retry")
			}
		}
		if task.Status == TaskFailed {
			if task.Kind == "image_generation" && task.Attempts >= task.MaxAttempts {
				task.Attempts = 0
				appendTaskLog(task, "手动重试，重置图片自动重试计数")
			}
			if task.Kind != "image_generation" && task.Attempts >= task.MaxAttempts {
				return fmt.Errorf("retry limit reached (%d attempts)", task.MaxAttempts)
			}
			task.ProviderOutputURL = ""
		}
		task.Status = TaskQueued
		task.Progress = 0
		task.Error = ""
		task.CompletedAt = nil
		task.UpdatedAt = time.Now().UTC()
		appendTaskLog(task, "任务重新排队")
		return nil
	}); err != nil {
		return err
	}
	return w.Enqueue(taskID)
}

func (w *Worker) loop(ctx context.Context) {
	defer w.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case taskID := <-w.queue:
			w.run(ctx, taskID)
		}
	}
}

func (w *Worker) imageLoop(ctx context.Context) {
	defer w.imageWG.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case taskID := <-w.imageQueue:
			w.run(ctx, taskID)
		}
	}
}

func (w *Worker) run(parent context.Context, taskID string) {
	ctx, cancel := context.WithCancel(parent)
	w.mu.Lock()
	if _, exists := w.running[taskID]; exists {
		w.mu.Unlock()
		cancel()
		return
	}
	w.running[taskID] = cancel
	w.mu.Unlock()
	defer func() {
		cancel()
		w.mu.Lock()
		delete(w.running, taskID)
		w.mu.Unlock()
	}()

	task, err := w.getTask(taskID)
	if err != nil || task.Status != TaskQueued {
		return
	}
	if task.Kind == "image_generation" {
		w.runImage(ctx, task)
		return
	}
	provider, ok := w.providers[task.Provider]
	if !ok {
		w.fail(taskID, fmt.Errorf("unknown provider %q", task.Provider))
		return
	}
	if err := w.updateTask(taskID, func(task *Task) {
		task.Status = TaskRunning
		task.Progress = 5
		appendTaskLog(task, "worker 开始处理")
	}); err != nil {
		return
	}

	if task.ProviderTaskID == "" {
		if task.Attempts >= task.MaxAttempts {
			w.fail(taskID, fmt.Errorf("retry limit reached (%d attempts)", task.MaxAttempts))
			return
		}
		task, _ = w.getTask(taskID)
		if task.Provider == "minimax" {
			prepared, err := w.prepareVideoTask(task)
			if err != nil {
				w.fail(taskID, err)
				return
			}
			task = prepared
		}
		if err := w.updateTask(taskID, func(task *Task) {
			task.Attempts++
			appendTaskLog(task, fmt.Sprintf("提交到 %s，第 %d 次", task.Provider, task.Attempts))
		}); err != nil {
			return
		}
		providerTaskID, err := provider.Submit(ctx, task)
		if err != nil {
			if ctx.Err() == nil {
				w.fail(taskID, fmt.Errorf("submit task: %w", err))
			}
			return
		}
		if err := w.updateTask(taskID, func(task *Task) {
			task.ProviderTaskID = providerTaskID
			task.Progress = 15
			appendTaskLog(task, "远端任务已接受: "+providerTaskID)
		}); err != nil {
			return
		}
		task.ProviderTaskID = providerTaskID
	}

	pollErrors := 0
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		result, err := provider.Poll(ctx, task.ProviderTaskID)
		if err != nil {
			pollErrors++
			_ = w.updateTask(taskID, func(task *Task) {
				appendTaskLog(task, fmt.Sprintf("查询失败 %d/3: %v", pollErrors, err))
			})
			if pollErrors >= 3 {
				w.fail(taskID, fmt.Errorf("poll task after 3 attempts: %w", err))
				return
			}
		} else {
			pollErrors = 0
			switch result.Status {
			case ProviderQueueing:
				_ = w.setProgress(taskID, 35, "远端排队中")
			case ProviderProcessing:
				_ = w.setProgress(taskID, 70, "远端生成中")
			case ProviderFailed:
				_ = w.updateTask(taskID, func(task *Task) {
					appendTaskLog(task, "远端任务失败；重试将新建远端任务")
					task.ProviderTaskID = ""
				})
				w.fail(taskID, errors.New("provider reported failure"))
				return
			case ProviderSuccess:
				if result.DownloadURL == "" {
					w.succeedWithoutFile(taskID)
					return
				}
				if err := w.completeVideo(ctx, taskID, result.DownloadURL); err != nil {
					if ctx.Err() == nil {
						w.fail(taskID, err)
					}
				}
				return
			default:
				w.fail(taskID, fmt.Errorf("unsupported provider status %q", result.Status))
				return
			}
		}
		timer := time.NewTimer(w.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (w *Worker) sequenceFrame(task Task) (VideoInput, error) {
	if task.PreviousTaskID == "" {
		return VideoInput{}, errors.New("sequence task has no previous task")
	}
	var previous Task
	var video Video
	err := w.store.View(func(state State) error {
		storedPrevious, err := findTask(&state, task.PreviousTaskID)
		if err != nil {
			return err
		}
		previous = *storedPrevious
		if previous.Status != TaskSucceeded || previous.VideoID == "" {
			return errors.New("previous sequence video is not ready")
		}
		for _, item := range state.Videos {
			if item.ID == previous.VideoID {
				video = item
				return nil
			}
		}
		return ErrNotFound
	})
	if err != nil {
		return VideoInput{}, fmt.Errorf("load previous sequence video: %w", err)
	}
	path, err := mediaPath(w.store.MediaDir(), video.StoragePath)
	if err != nil {
		return VideoInput{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-sseof", "-0.1", "-i", path, "-frames:v", "1", "-f", "image2pipe", "-vcodec", "png", "pipe:1").Output()
	if err != nil {
		return VideoInput{}, fmt.Errorf("extract previous video last frame: %w", err)
	}
	if len(output) == 0 || len(output) > maxSingleInputImage {
		return VideoInput{}, errors.New("previous video last frame has invalid size")
	}
	return VideoInput{Role: "first_frame", ContentType: "image/png", Data: output}, nil
}

func (w *Worker) prepareVideoTask(task Task) (Task, error) {
	if task.PreviousTaskID != "" {
		input, err := w.sequenceFrame(task)
		if err != nil {
			return Task{}, fmt.Errorf("prepare sequence first frame: %w", err)
		}
		task.VideoInputs = []VideoInput{input}
	}
	err := w.store.View(func(state State) error {
		shot, err := findShot(&state, task.ShotID)
		if err != nil {
			return err
		}
		if task.CharacterPromptCount > 0 {
			addition, err := characterPrompt(w.store.MediaDir(), state, *shot, task.CharacterPromptCount)
			if err != nil {
				return err
			}
			task.Prompt += addition
		}
		if task.PreviousTaskID != "" {
			return nil
		}
		if task.UseFrameImages {
			inputs, err := frameImages(w.store.MediaDir(), state, *shot)
			if err != nil {
				return err
			}
			task.VideoInputs = inputs
		} else if len(task.ReferenceImageIDs) > 0 {
			inputs, err := referenceImages(w.store.MediaDir(), state, task)
			if err != nil {
				return err
			}
			task.VideoInputs = inputs
		}
		return nil
	})
	if err != nil {
		return Task{}, fmt.Errorf("prepare video inputs: %w", err)
	}
	return task, nil
}

func referenceImages(mediaDir string, state State, task Task) ([]VideoInput, error) {
	inputs := make([]VideoInput, 0, len(task.ReferenceImageIDs))
	var totalBytes int
	for _, assetID := range task.ReferenceImageIDs {
		asset, err := findAsset(&state, assetID)
		if err != nil {
			return nil, fmt.Errorf("reference image %q: %w", assetID, err)
		}
		if asset.ProjectID != task.ProjectID || !isVideoInputImage(*asset) {
			return nil, fmt.Errorf("reference image %q is not an eligible project image", assetID)
		}
		input, err := readVideoInput(mediaDir, *asset, "reference_image")
		if err != nil {
			return nil, err
		}
		totalBytes += len(input.Data)
		if totalBytes > maxVideoInputBytes {
			return nil, errors.New("reference images exceed the MiniMax request size limit")
		}
		inputs = append(inputs, input)
	}
	return inputs, nil
}

func characterPrompt(mediaDir string, state State, shot Shot, limit int) (string, error) {
	models := []string{}
	for _, link := range state.AssetLinks {
		if link.TargetType != "shot" || link.TargetID != shot.ID {
			continue
		}
		asset, err := findAsset(&state, link.AssetID)
		if err != nil || asset.Kind != "character" {
			continue
		}
		path, err := mediaPath(mediaDir, asset.StoragePath)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read character model %q: %w", asset.Name, err)
		}
		if len(data) > maxTextPreviewBytes {
			return "", fmt.Errorf("character model %q exceeds 1 MiB", asset.Name)
		}
		models = append(models, string(data))
		if len(models) == limit {
			break
		}
	}
	if len(models) == 0 {
		return "", errors.New("no linked character models available for prompt")
	}
	addition := "\n\n角色模型约束（必须遵守）：\n" + strings.Join(models, "\n\n")
	if len([]rune(shot.Prompt+addition)) > 7000 {
		return "", errors.New("character models make prompt exceed MiniMax 7000-character limit")
	}
	return addition, nil
}

func frameImages(mediaDir string, state State, shot Shot) ([]VideoInput, error) {
	roles := map[string]VideoInput{}
	for _, link := range state.AssetLinks {
		if link.TargetType != "shot" || link.TargetID != shot.ID || !strings.HasPrefix(link.Note, "GPT Image 2 ") {
			continue
		}
		role := ""
		switch strings.TrimPrefix(link.Note, "GPT Image 2 ") {
		case "首帧图":
			role = "first_frame"
		case "末帧图":
			role = "last_frame"
		}
		if role == "" || roles[role].Data != nil {
			continue
		}
		asset, err := findAsset(&state, link.AssetID)
		if err != nil || !isVideoInputImage(*asset) {
			continue
		}
		input, err := readVideoInput(mediaDir, *asset, role)
		if err != nil {
			return nil, err
		}
		roles[role] = input
	}
	first, hasFirst := roles["first_frame"]
	last, hasLast := roles["last_frame"]
	if !hasFirst || !hasLast {
		return nil, errors.New("first_last_frame shot needs generated first and last frame images")
	}
	return []VideoInput{first, last}, nil
}

func isVideoInputImage(asset Asset) bool {
	return slices.Contains([]string{"image/jpeg", "image/png", "image/webp"}, asset.ContentType) &&
		asset.Size > 0 && asset.Size <= maxSingleInputImage
}

func readVideoInput(mediaDir string, asset Asset, role string) (VideoInput, error) {
	path, err := mediaPath(mediaDir, asset.StoragePath)
	if err != nil {
		return VideoInput{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return VideoInput{}, fmt.Errorf("read %s: %w", role, err)
	}
	if len(data) == 0 || len(data) > maxSingleInputImage {
		return VideoInput{}, fmt.Errorf("%s image has invalid size", role)
	}
	return VideoInput{Role: role, ContentType: asset.ContentType, Data: data}, nil
}

func mediaPath(mediaDir, storagePath string) (string, error) {
	if !filepath.IsLocal(filepath.FromSlash(storagePath)) {
		return "", errors.New("invalid storage path")
	}
	path := filepath.Join(mediaDir, filepath.FromSlash(storagePath))
	rel, err := filepath.Rel(mediaDir, path)
	if err != nil || !filepath.IsLocal(rel) {
		return "", errors.New("storage path escapes media directory")
	}
	return path, nil
}

func (w *Worker) runImage(ctx context.Context, task Task) {
	provider, ok := w.imageProviders[task.Provider]
	if !ok {
		w.fail(task.ID, fmt.Errorf("unknown image provider %q", task.Provider))
		return
	}
	if task.Attempts >= task.MaxAttempts {
		w.fail(task.ID, fmt.Errorf("retry limit reached (%d attempts)", task.MaxAttempts))
		return
	}
	if err := w.updateTask(task.ID, func(task *Task) {
		task.Status = TaskRunning
		task.Progress = 10
		task.Attempts++
		appendTaskLog(task, fmt.Sprintf("提交到 %s，第 %d 次", task.Provider, task.Attempts))
	}); err != nil {
		return
	}
	image, contentType, err := provider.Generate(ctx, task)
	if err != nil {
		if ctx.Err() == nil {
			w.fail(task.ID, fmt.Errorf("generate image: %w", err))
		}
		return
	}
	if err := w.completeImage(task.ID, image, contentType); err != nil && ctx.Err() == nil {
		w.fail(task.ID, err)
	}
}

func (w *Worker) completeImage(taskID string, image []byte, contentType string) error {
	if len(image) == 0 || len(image) > 20<<20 {
		return errors.New("image has invalid size")
	}
	task, err := w.getTask(taskID)
	if err != nil {
		return err
	}
	targetID := task.ShotID
	if targetID == "" {
		targetID = task.AssetID
	}
	if targetID == "" {
		return errors.New("image task has no target")
	}
	filename := targetID + "-" + task.ImageRole + ".png"
	dir := filepath.Join(w.store.MediaDir(), task.ProjectID, "assets", taskID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create image directory: %w", err)
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, image, 0o600); err != nil {
		return fmt.Errorf("store image: %w", err)
	}
	assetID, err := newID("ast")
	if err != nil {
		return err
	}
	linkID, err := newID("lnk")
	if err != nil {
		return err
	}
	relPath, err := filepath.Rel(w.store.MediaDir(), path)
	if err != nil {
		return fmt.Errorf("resolve image path: %w", err)
	}
	return w.store.Update(func(state *State) error {
		storedTask, err := findTask(state, taskID)
		if err != nil {
			return err
		}
		if storedTask.Status == TaskCancelled {
			return context.Canceled
		}
		now := time.Now().UTC()
		name := imageRoleName(task.ImageRole)
		targetType := "shot"
		linkNote := "GPT Image 2 " + name
		if task.AssetID != "" {
			character, err := findAsset(state, task.AssetID)
			if err != nil {
				return err
			}
			name = character.Name + " · " + imageRoleName(task.ImageRole)
			targetType = "asset"
			linkNote = "GPT Image 2 角色" + imageRoleName(task.ImageRole)
		}
		state.Assets = append(state.Assets, Asset{
			ID: assetID, ProjectID: task.ProjectID, Name: name, Kind: "image",
			Filename: filename, ContentType: contentType, Size: int64(len(image)),
			StoragePath: filepath.ToSlash(relPath), CreatedAt: now,
		})
		state.AssetLinks = append(state.AssetLinks, AssetLink{
			ID: linkID, ProjectID: task.ProjectID, AssetID: assetID, TargetType: targetType,
			TargetID: targetID, Note: linkNote, CreatedAt: now,
		})
		storedTask.Status = TaskSucceeded
		storedTask.Progress = 100
		storedTask.UpdatedAt = now
		storedTask.CompletedAt = &now
		appendTaskLog(storedTask, "图片已生成并关联"+map[string]string{"shot": "镜头", "asset": "角色素材"}[targetType])
		return nil
	})
}

func imageRoleName(role string) string {
	switch role {
	case "first-frame":
		return "首帧图"
	case "last-frame":
		return "末帧图"
	case "preview":
		return "预览图"
	case "effect-front":
		return "正面效果图"
	case "effect-profile":
		return "侧面效果图"
	case "effect-action":
		return "动作效果图"
	default:
		return "镜头图片"
	}
}

func (w *Worker) getTask(id string) (Task, error) {
	var result Task
	err := w.store.View(func(state State) error {
		for _, task := range state.Tasks {
			if task.ID == id {
				result = task
				return nil
			}
		}
		return ErrNotFound
	})
	return result, err
}

func (w *Worker) updateTask(id string, update func(*Task)) error {
	return w.store.Update(func(state *State) error {
		task, err := findTask(state, id)
		if err != nil {
			return err
		}
		if task.Status == TaskCancelled {
			return context.Canceled
		}
		update(task)
		task.UpdatedAt = time.Now().UTC()
		return nil
	})
}

func (w *Worker) setProgress(id string, progress int, message string) error {
	return w.updateTask(id, func(task *Task) {
		if progress > task.Progress {
			task.Progress = progress
		}
		if len(task.Logs) == 0 || !strings.HasSuffix(task.Logs[len(task.Logs)-1], message) {
			appendTaskLog(task, message)
		}
	})
}

func (w *Worker) fail(id string, cause error) {
	if err := w.store.Update(func(state *State) error {
		task, err := findTask(state, id)
		if err != nil || task.Status == TaskCancelled {
			return err
		}
		now := time.Now().UTC()
		task.Status = TaskFailed
		task.Error = cause.Error()
		task.UpdatedAt = now
		task.CompletedAt = &now
		appendTaskLog(task, "失败: "+cause.Error())
		blockedBy := map[string]bool{id: true}
		for changed := true; changed; {
			changed = false
			for i := range state.Tasks {
				candidate := &state.Tasks[i]
				if !blockedBy[candidate.PreviousTaskID] || candidate.Status != TaskQueued {
					continue
				}
				candidate.Status = TaskCancelled
				candidate.Error = "previous sequence task failed"
				candidate.UpdatedAt = now
				candidate.CompletedAt = &now
				appendTaskLog(candidate, "前序试片失败，后续镜头未提交")
				blockedBy[candidate.ID] = true
				changed = true
			}
		}
		return nil
	}); err != nil && !errors.Is(err, ErrNotFound) {
		w.logger.Error("persist task failure", "task_id", id, "error", err)
	}
}

func (w *Worker) succeedWithoutFile(id string) {
	if err := w.store.Update(func(state *State) error {
		task, err := findTask(state, id)
		if err != nil || task.Status == TaskCancelled {
			return err
		}
		now := time.Now().UTC()
		task.Status = TaskSucceeded
		task.Progress = 100
		task.UpdatedAt = now
		task.CompletedAt = &now
		appendTaskLog(task, "任务完成（模拟模式无视频文件）")
		return nil
	}); err != nil && !errors.Is(err, ErrNotFound) {
		w.logger.Error("persist task completion", "task_id", id, "error", err)
	}
}

func (w *Worker) completeVideo(ctx context.Context, taskID, downloadURL string) error {
	if err := w.setProgress(taskID, 85, "生成完成，准备下载"); err != nil {
		return err
	}
	task, err := w.getTask(taskID)
	if err != nil {
		return err
	}
	filename := taskID + ".mp4"
	dir := filepath.Join(w.store.MediaDir(), task.ProjectID, "videos", taskID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create video directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return fmt.Errorf("create video temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("create video download request: %w", err)
	}
	resp, err := w.downloadClient.Do(req)
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("download video: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = tmp.Close()
		return fmt.Errorf("download video returned %s", resp.Status)
	}
	if resp.ContentLength > w.maxDownloadBytes {
		_ = tmp.Close()
		return fmt.Errorf("video exceeds download limit of %d bytes", w.maxDownloadBytes)
	}
	written, err := io.Copy(tmp, io.LimitReader(resp.Body, w.maxDownloadBytes+1))
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write video: %w", err)
	}
	if written > w.maxDownloadBytes {
		_ = tmp.Close()
		return fmt.Errorf("video exceeds download limit of %d bytes", w.maxDownloadBytes)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync video: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close video: %w", err)
	}
	finalPath := filepath.Join(dir, filename)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("store video: %w", err)
	}
	videoID, err := newID("vid")
	if err != nil {
		return err
	}
	relPath, err := filepath.Rel(w.store.MediaDir(), finalPath)
	if err != nil {
		return fmt.Errorf("resolve video path: %w", err)
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(filename))
	}
	var nextTaskID string
	err = w.store.Update(func(state *State) error {
		storedTask, err := findTask(state, taskID)
		if err != nil {
			return err
		}
		if storedTask.Status == TaskCancelled {
			return context.Canceled
		}
		now := time.Now().UTC()
		state.Videos = append(state.Videos, Video{
			ID: videoID, ProjectID: task.ProjectID, TaskID: taskID, ShotID: task.ShotID,
			Title: task.Prompt, Filename: filename, ContentType: contentType,
			Size: written, StoragePath: filepath.ToSlash(relPath),
			Provider: task.Provider, Model: task.Model, CreatedAt: now,
		})
		storedTask.Status = TaskSucceeded
		storedTask.Progress = 100
		storedTask.ProviderOutputURL = downloadURL
		storedTask.VideoID = videoID
		if storedTask.ShotID != "" {
			if shot, err := findShot(state, storedTask.ShotID); err == nil {
				shot.VideoID = videoID
				shot.TaskID = taskID
				shot.UpdatedAt = now
			}
		}
		storedTask.UpdatedAt = now
		storedTask.CompletedAt = &now
		appendTaskLog(storedTask, "视频已下载并入库")
		for i := range state.Tasks {
			candidate := &state.Tasks[i]
			if candidate.PreviousTaskID == taskID && candidate.Status == TaskQueued {
				nextTaskID = candidate.ID
				appendTaskLog(candidate, "前序视频已完成，等待提取末帧后提交")
				break
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if nextTaskID != "" {
		if err := w.Enqueue(nextTaskID); err != nil {
			w.fail(nextTaskID, fmt.Errorf("enqueue after previous sequence video: %w", err))
		}
	}
	return nil
}

func newShotImageTasks(state *State, shot Shot) ([]Task, int, error) {
	tasks := make([]Task, 0, 3)
	skipped := 0
	for _, role := range []string{"first-frame", "last-frame", "preview"} {
		if hasShotImageTask(*state, shot.ID, role) {
			skipped++
			continue
		}
		id, err := newID("tsk")
		if err != nil {
			return nil, 0, err
		}
		now := time.Now().UTC()
		task := Task{
			ID: id, ProjectID: shot.ProjectID, ShotID: shot.ID, Kind: "image_generation",
			Provider: "openai", Model: "gpt-image-2", Prompt: imagePrompt(shot, role), ImageRole: role,
			AspectRatio: shot.AspectRatio, Status: TaskQueued, MaxAttempts: 2, CreatedAt: now, UpdatedAt: now,
		}
		appendTaskLog(&task, "创建 "+imageRoleName(role))
		tasks = append(tasks, task)
	}
	return tasks, skipped, nil
}

func newCharacterImageTasks(state *State, character Asset, model string) ([]Task, int, error) {
	tasks := make([]Task, 0, 4)
	skipped := 0
	for _, role := range []string{"preview", "effect-front", "effect-profile", "effect-action"} {
		if hasCharacterImageTask(*state, character.ID, role) {
			skipped++
			continue
		}
		id, err := newID("tsk")
		if err != nil {
			return nil, 0, err
		}
		now := time.Now().UTC()
		task := Task{
			ID: id, ProjectID: character.ProjectID, AssetID: character.ID, Kind: "image_generation",
			Provider: "openai", Model: "gpt-image-2", Prompt: characterImagePrompt(character, model, role), ImageRole: role,
			AspectRatio: "1:1", Status: TaskQueued, MaxAttempts: 2, CreatedAt: now, UpdatedAt: now,
		}
		appendTaskLog(&task, "创建角色"+imageRoleName(role))
		tasks = append(tasks, task)
	}
	return tasks, skipped, nil
}

func hasCharacterImageTask(state State, assetID, role string) bool {
	for _, task := range state.Tasks {
		if task.AssetID == assetID && task.Kind == "image_generation" && task.ImageRole == role &&
			(task.Status == TaskQueued || task.Status == TaskRunning || task.Status == TaskSucceeded) {
			return true
		}
	}
	return false
}

func characterImagePrompt(character Asset, model, role string) string {
	roleInstruction := "Create a clean square character key art preview, full body, neutral three-quarter standing pose, readable face and silhouette."
	if strings.HasPrefix(character.Filename, "group-") {
		roleInstruction = "Create a clean square unit key art preview showing a readable lineup of the distinct group members, with correct species, scale, equipment, and formation."
	}
	switch role {
	case "effect-front":
		roleInstruction = "Create a square full-body front-view character turnaround effect image, neutral stance, readable costume, equipment, face, hair, and silhouette."
		if strings.HasPrefix(character.Filename, "group-") {
			roleInstruction = "Create a square front-facing unit lineup effect image, showing distinct group members, correct relative scale, equipment, and formation."
		}
	case "effect-profile":
		roleInstruction = "Create a square full-body side-profile character turnaround effect image, neutral stance, readable costume layers, equipment, hair, and silhouette."
		if strings.HasPrefix(character.Filename, "group-") {
			roleInstruction = "Create a square side-profile unit formation effect image, showing distinct group members, correct relative scale, equipment, and spacing."
		}
	case "effect-action":
		roleInstruction = "Create a square cinematic character effect image showing one natural signature action and baseline personality, with full body readable and no combat impact."
		if strings.HasPrefix(character.Filename, "group-") {
			roleInstruction = "Create a square cinematic unit effect image showing one coordinated signature formation action, with all members readable and no combat impact."
		}
	}
	return roleInstruction +
		"\n\nCharacter asset: " + character.Name +
		"\n\nAuthoritative character model JSON:\n" + model +
		"\n\nKeep exact identity, age, body scale, face, hair, eye color, costume, equipment, and timeline restrictions. Plain restrained background. No text, labels, logos, watermarks, collage, split panels, duplicate character, injury, blood, or gore."
}

func hasShotImageTask(state State, shotID, role string) bool {
	for _, task := range state.Tasks {
		if task.ShotID == shotID && task.Kind == "image_generation" && task.ImageRole == role &&
			(task.Status == TaskQueued || task.Status == TaskRunning || task.Status == TaskSucceeded) {
			return true
		}
	}
	return false
}

func imagePrompt(shot Shot, role string) string {
	roleInstruction := "Create a clean, compelling preview image that represents this shot."
	if role == "first-frame" {
		roleInstruction = "Create first frame of this shot, before motion begins. Preserve composition, character identity, costume, location, and lighting."
	}
	if role == "last-frame" {
		roleInstruction = "Create final frame of this shot, after described action completes. Preserve composition, character identity, costume, location, and lighting."
	}
	return roleInstruction + "\n\nShot visual: " + shot.Visual + "\n\nProduction prompt:\n" + shot.Prompt + "\n\nNo text, no logos, no watermarks."
}

func safeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == '.' || r == ' ' ||
			(r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r > 127 {
			return r
		}
		return '_'
	}, name)
	if name == "." || name == ".." {
		return ""
	}
	return name
}
