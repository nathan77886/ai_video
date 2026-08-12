package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type ProviderStatus string

const (
	ProviderPreparing  ProviderStatus = "preparing"
	ProviderQueueing   ProviderStatus = "queueing"
	ProviderProcessing ProviderStatus = "processing"
	ProviderSuccess    ProviderStatus = "success"
	ProviderFailed     ProviderStatus = "failed"
)

type ProviderResult struct {
	Status ProviderStatus
	FileID string
}

type DownloadInfo struct {
	URL      string
	Filename string
	Size     int64
}

type VideoProvider interface {
	Submit(context.Context, Task) (string, error)
	Poll(context.Context, string) (ProviderResult, error)
	Retrieve(context.Context, string) (DownloadInfo, error)
}

type MockProvider struct{}

func (MockProvider) Submit(_ context.Context, _ Task) (string, error) {
	return "mock:" + strconv.FormatInt(time.Now().UnixNano(), 10), nil
}

func (MockProvider) Poll(_ context.Context, id string) (ProviderResult, error) {
	raw, ok := strings.CutPrefix(id, "mock:")
	if !ok {
		return ProviderResult{}, errors.New("invalid mock task id")
	}
	started, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return ProviderResult{}, fmt.Errorf("parse mock task id: %w", err)
	}
	elapsed := time.Since(time.Unix(0, started))
	switch {
	case elapsed < time.Second:
		return ProviderResult{Status: ProviderQueueing}, nil
	case elapsed < 2*time.Second:
		return ProviderResult{Status: ProviderProcessing}, nil
	default:
		return ProviderResult{Status: ProviderSuccess}, nil
	}
}

func (MockProvider) Retrieve(_ context.Context, _ string) (DownloadInfo, error) {
	return DownloadInfo{}, errors.New("mock provider has no output file")
}

type MiniMaxProvider struct {
	baseURL string
	apiKey  string
	allowed bool
	client  *http.Client
}

func NewMiniMaxProvider(baseURL, apiKey string, allowed bool) (*MiniMaxProvider, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.minimax.io"
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil ||
		(u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname()))) {
		return nil, errors.New("invalid MiniMax base URL")
	}
	return &MiniMaxProvider{
		baseURL: baseURL,
		apiKey:  strings.TrimSpace(apiKey),
		allowed: allowed,
		client:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (p *MiniMaxProvider) Submit(ctx context.Context, task Task) (string, error) {
	if !p.allowed {
		return "", errors.New("paid generation disabled; set ALLOW_PAID_GENERATION=true")
	}
	if p.apiKey == "" {
		return "", errors.New("MINIMAX_API_KEY is not configured")
	}
	payload := map[string]any{
		"model":            task.Model,
		"prompt":           task.Prompt,
		"prompt_optimizer": true,
	}
	if task.FirstFrameImage != "" {
		payload["first_frame_image"] = task.FirstFrameImage
	}
	if task.Duration != 0 {
		payload["duration"] = task.Duration
	}
	if task.Resolution != "" {
		payload["resolution"] = task.Resolution
	}
	var response struct {
		TaskID   string   `json:"task_id"`
		BaseResp baseResp `json:"base_resp"`
	}
	if err := p.requestJSON(ctx, http.MethodPost, "/v1/video_generation", payload, &response); err != nil {
		return "", err
	}
	if err := response.BaseResp.err(); err != nil {
		return "", err
	}
	if response.TaskID == "" {
		return "", errors.New("MiniMax response missing task_id")
	}
	return response.TaskID, nil
}

func (p *MiniMaxProvider) Poll(ctx context.Context, taskID string) (ProviderResult, error) {
	if p.apiKey == "" {
		return ProviderResult{}, errors.New("MINIMAX_API_KEY is not configured")
	}
	var response struct {
		Status   string   `json:"status"`
		FileID   string   `json:"file_id"`
		BaseResp baseResp `json:"base_resp"`
	}
	path := "/v1/query/video_generation?task_id=" + url.QueryEscape(taskID)
	if err := p.requestJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return ProviderResult{}, err
	}
	if err := response.BaseResp.err(); err != nil {
		return ProviderResult{}, err
	}
	status := strings.ToLower(response.Status)
	switch status {
	case "preparing":
		return ProviderResult{Status: ProviderPreparing}, nil
	case "queueing":
		return ProviderResult{Status: ProviderQueueing}, nil
	case "processing":
		return ProviderResult{Status: ProviderProcessing}, nil
	case "success":
		return ProviderResult{Status: ProviderSuccess, FileID: response.FileID}, nil
	case "fail", "failed":
		return ProviderResult{Status: ProviderFailed}, nil
	default:
		return ProviderResult{}, fmt.Errorf("unknown MiniMax task status %q", response.Status)
	}
}

func (p *MiniMaxProvider) Retrieve(ctx context.Context, fileID string) (DownloadInfo, error) {
	if p.apiKey == "" {
		return DownloadInfo{}, errors.New("MINIMAX_API_KEY is not configured")
	}
	var response struct {
		File struct {
			Filename    string `json:"filename"`
			Bytes       int64  `json:"bytes"`
			DownloadURL string `json:"download_url"`
		} `json:"file"`
		BaseResp baseResp `json:"base_resp"`
	}
	path := "/v1/files/retrieve?file_id=" + url.QueryEscape(fileID)
	if err := p.requestJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return DownloadInfo{}, err
	}
	if err := response.BaseResp.err(); err != nil {
		return DownloadInfo{}, err
	}
	u, err := url.Parse(response.File.DownloadURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return DownloadInfo{}, errors.New("MiniMax response contains invalid download URL")
	}
	return DownloadInfo{
		URL:      response.File.DownloadURL,
		Filename: response.File.Filename,
		Size:     response.File.Bytes,
	}, nil
}

func isLoopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

type baseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

func (r baseResp) err() error {
	if r.StatusCode == 0 {
		return nil
	}
	return fmt.Errorf("MiniMax API error %d: %s", r.StatusCode, r.StatusMsg)
}

func (p *MiniMaxProvider) requestJSON(ctx context.Context, method, path string, body any, target any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode MiniMax request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("create MiniMax request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("call MiniMax API: %w", err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, 2<<20)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(limited)
		return fmt.Errorf("MiniMax API returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	if err := json.NewDecoder(limited).Decode(target); err != nil {
		return fmt.Errorf("decode MiniMax response: %w", err)
	}
	return nil
}
