package app

import "time"

type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Script struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Asset struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	StoragePath string    `json:"storage_path"`
	CreatedAt   time.Time `json:"created_at"`
}

type Video struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	TaskID      string    `json:"task_id,omitempty"`
	Title       string    `json:"title"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	StoragePath string    `json:"storage_path"`
	Provider    string    `json:"provider,omitempty"`
	Model       string    `json:"model,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type TaskStatus string

const (
	TaskQueued    TaskStatus = "queued"
	TaskRunning   TaskStatus = "running"
	TaskSucceeded TaskStatus = "succeeded"
	TaskFailed    TaskStatus = "failed"
	TaskCancelled TaskStatus = "cancelled"
)

type Task struct {
	ID              string     `json:"id"`
	ProjectID       string     `json:"project_id"`
	Kind            string     `json:"kind"`
	Provider        string     `json:"provider"`
	Model           string     `json:"model"`
	Prompt          string     `json:"prompt"`
	FirstFrameImage string     `json:"first_frame_image,omitempty"`
	Duration        int        `json:"duration,omitempty"`
	Resolution      string     `json:"resolution,omitempty"`
	Status          TaskStatus `json:"status"`
	Progress        int        `json:"progress"`
	ProviderTaskID  string     `json:"provider_task_id,omitempty"`
	ProviderFileID  string     `json:"provider_file_id,omitempty"`
	VideoID         string     `json:"video_id,omitempty"`
	Attempts        int        `json:"attempts"`
	MaxAttempts     int        `json:"max_attempts"`
	Error           string     `json:"error,omitempty"`
	Logs            []string   `json:"logs,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

type State struct {
	Projects []Project `json:"projects"`
	Scripts  []Script  `json:"scripts"`
	Assets   []Asset   `json:"assets"`
	Videos   []Video   `json:"videos"`
	Tasks    []Task    `json:"tasks"`
}
