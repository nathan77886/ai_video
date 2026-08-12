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

type AssetLink struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	AssetID    string    `json:"asset_id"`
	TargetType string    `json:"target_type"`
	TargetID   string    `json:"target_id"`
	Note       string    `json:"note,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type AssetWithLinks struct {
	Asset
	Links []AssetLink `json:"links"`
}

type ShotReviewStatus string

const (
	ShotPending          ShotReviewStatus = "pending"
	ShotApproved         ShotReviewStatus = "approved"
	ShotChangesRequested ShotReviewStatus = "changes_requested"
	ShotRejected         ShotReviewStatus = "rejected"
)

type Shot struct {
	ID                     string           `json:"id"`
	ProjectID              string           `json:"project_id"`
	EpisodeID              string           `json:"episode_id"`
	SceneID                string           `json:"scene_id"`
	Chapter                int              `json:"chapter"`
	ChapterTitle           string           `json:"chapter_title"`
	Framing                string           `json:"framing"`
	Camera                 string           `json:"camera"`
	Visual                 string           `json:"visual"`
	Audio                  string           `json:"audio"`
	SourceMode             string           `json:"source_mode"`
	Duration               int              `json:"duration_sec"`
	AspectRatio            string           `json:"aspect_ratio"`
	TargetModel            string           `json:"target_model"`
	GenerationRoute        string           `json:"generation_route"`
	RequiresEditorialSplit bool             `json:"requires_editorial_split"`
	Prompt                 string           `json:"prompt"`
	NegativePrompt         string           `json:"negative_prompt"`
	InputVersion           string           `json:"input_version"`
	ReviewStatus           ShotReviewStatus `json:"review_status"`
	ReviewNote             string           `json:"review_note,omitempty"`
	TaskID                 string           `json:"task_id,omitempty"`
	VideoID                string           `json:"video_id,omitempty"`
	CreatedAt              time.Time        `json:"created_at"`
	UpdatedAt              time.Time        `json:"updated_at"`
}

type ShotWithAssets struct {
	Shot
	AssetLinks []AssetLink `json:"asset_links"`
}

type Video struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	TaskID      string    `json:"task_id,omitempty"`
	ShotID      string    `json:"shot_id,omitempty"`
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
	ID                string     `json:"id"`
	ProjectID         string     `json:"project_id"`
	ShotID            string     `json:"shot_id,omitempty"`
	AssetID           string     `json:"asset_id,omitempty"`
	Kind              string     `json:"kind"`
	Provider          string     `json:"provider"`
	Model             string     `json:"model"`
	Prompt            string     `json:"prompt"`
	ImageRole         string     `json:"image_role,omitempty"`
	Duration          int        `json:"duration,omitempty"`
	Resolution        string     `json:"resolution,omitempty"`
	AspectRatio       string     `json:"aspect_ratio,omitempty"`
	Status            TaskStatus `json:"status"`
	Progress          int        `json:"progress"`
	ProviderTaskID    string     `json:"provider_task_id,omitempty"`
	ProviderOutputURL string     `json:"provider_output_url,omitempty"`
	VideoID           string     `json:"video_id,omitempty"`
	Attempts          int        `json:"attempts"`
	MaxAttempts       int        `json:"max_attempts"`
	Error             string     `json:"error,omitempty"`
	Logs              []string   `json:"logs,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	UseFrameImages       bool         `json:"use_frame_images,omitempty"`
	CharacterPromptCount int          `json:"character_prompt_count,omitempty"`
	ReferenceImageIDs    []string     `json:"reference_image_ids,omitempty"`
	PreviousTaskID       string       `json:"previous_task_id,omitempty"`
	VideoInputs          []VideoInput `json:"-"`
}

type VideoInput struct {
	Role        string
	ContentType string
	Data        []byte
}

type State struct {
	Projects   []Project   `json:"projects"`
	Scripts    []Script    `json:"scripts"`
	Shots      []Shot      `json:"shots"`
	Assets     []Asset     `json:"assets"`
	AssetLinks []AssetLink `json:"asset_links"`
	Videos     []Video     `json:"videos"`
	Tasks      []Task      `json:"tasks"`
}
