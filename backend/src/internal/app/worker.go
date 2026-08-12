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
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Worker struct {
	store            *Store
	providers        map[string]VideoProvider
	queue            chan string
	pollInterval     time.Duration
	maxDownloadBytes int64
	downloadClient   *http.Client
	logger           *slog.Logger
	mu               sync.Mutex
	running          map[string]context.CancelFunc
	wg               sync.WaitGroup
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
		queue:            make(chan string, 100),
		pollInterval:     pollInterval,
		maxDownloadBytes: maxDownloadBytes,
		downloadClient:   &http.Client{Timeout: 20 * time.Minute},
		logger:           logger,
		running:          make(map[string]context.CancelFunc),
	}
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
	for _, id := range queued {
		if err := w.Enqueue(id); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) Wait() { w.wg.Wait() }

func (w *Worker) Enqueue(taskID string) error {
	select {
	case w.queue <- taskID:
		return nil
	default:
		return errors.New("task queue is full")
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
		appendTaskLog(task, "本地任务已取消；MiniMax v1 远端任务不会被终止")
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
		if task.Status == TaskFailed {
			if task.Attempts >= task.MaxAttempts {
				return fmt.Errorf("retry limit reached (%d attempts)", task.MaxAttempts)
			}
			task.ProviderFileID = ""
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
		if err := w.updateTask(taskID, func(task *Task) {
			task.Attempts++
			appendTaskLog(task, fmt.Sprintf("提交到 %s，第 %d 次", task.Provider, task.Attempts))
		}); err != nil {
			return
		}
		task, _ = w.getTask(taskID)
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
			case ProviderPreparing:
				_ = w.setProgress(taskID, 25, "远端准备中")
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
				if result.FileID == "" {
					w.succeedWithoutFile(taskID)
					return
				}
				if err := w.completeVideo(ctx, taskID, provider, result.FileID); err != nil {
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

func (w *Worker) completeVideo(ctx context.Context, taskID string, provider VideoProvider, fileID string) error {
	if err := w.setProgress(taskID, 85, "生成完成，准备下载"); err != nil {
		return err
	}
	info, err := provider.Retrieve(ctx, fileID)
	if err != nil {
		return fmt.Errorf("retrieve output: %w", err)
	}
	if info.Size > w.maxDownloadBytes {
		return fmt.Errorf("video exceeds download limit of %d bytes", w.maxDownloadBytes)
	}
	task, err := w.getTask(taskID)
	if err != nil {
		return err
	}
	filename := safeFilename(info.Filename)
	if filename == "" {
		filename = taskID + ".mp4"
	}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
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
	return w.store.Update(func(state *State) error {
		storedTask, err := findTask(state, taskID)
		if err != nil {
			return err
		}
		if storedTask.Status == TaskCancelled {
			return context.Canceled
		}
		now := time.Now().UTC()
		state.Videos = append(state.Videos, Video{
			ID: videoID, ProjectID: task.ProjectID, TaskID: taskID,
			Title: task.Prompt, Filename: filename, ContentType: contentType,
			Size: written, StoragePath: filepath.ToSlash(relPath),
			Provider: task.Provider, Model: task.Model, CreatedAt: now,
		})
		storedTask.Status = TaskSucceeded
		storedTask.Progress = 100
		storedTask.ProviderFileID = fileID
		storedTask.VideoID = videoID
		storedTask.UpdatedAt = now
		storedTask.CompletedAt = &now
		appendTaskLog(storedTask, "视频已下载并入库")
		return nil
	})
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
