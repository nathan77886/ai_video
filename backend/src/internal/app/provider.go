package app

import (
	"bytes"
	"context"
	"encoding/base64"
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
	ProviderQueueing   ProviderStatus = "queueing"
	ProviderProcessing ProviderStatus = "processing"
	ProviderSuccess    ProviderStatus = "success"
	ProviderFailed     ProviderStatus = "failed"
)

type ProviderResult struct {
	Status      ProviderStatus
	DownloadURL string
}

type VideoProvider interface {
	Submit(context.Context, Task) (string, error)
	Poll(context.Context, string) (ProviderResult, error)
}

type ImageProvider interface {
	Generate(context.Context, Task) ([]byte, string, error)
}

type OpenAIImageProvider struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

const imageRequestTimeout = 8 * time.Minute

func NewOpenAIImageProvider(baseURL, apiKey string) (*OpenAIImageProvider, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("OpenAI image base URL is required")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil ||
		(u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname()))) {
		return nil, errors.New("invalid OpenAI image base URL")
	}
	return &OpenAIImageProvider{
		baseURL: baseURL,
		apiKey:  strings.TrimSpace(apiKey),
		client:  &http.Client{Timeout: imageRequestTimeout},
	}, nil
}

func (p *OpenAIImageProvider) Generate(ctx context.Context, task Task) ([]byte, string, error) {
	if p.apiKey == "" {
		return nil, "", errors.New("OPENAI_API_KEY is not configured")
	}
	payload := map[string]any{
		"model":         "gpt-image-2",
		"prompt":        imageGenerationPrompt(task),
		"size":          imageSize(task.AspectRatio),
		"quality":       "low",
		"output_format": "png",
		"moderation":    "auto",
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("encode OpenAI image request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/images/generations", bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("create OpenAI image request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("call OpenAI image API: %w", err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, 20<<20)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(limited)
		return nil, "", fmt.Errorf("OpenAI image API returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	var response struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.NewDecoder(limited).Decode(&response); err != nil {
		return nil, "", fmt.Errorf("decode OpenAI image response: %w", err)
	}
	if len(response.Data) != 1 || response.Data[0].B64JSON == "" {
		return nil, "", errors.New("OpenAI image response missing image data")
	}
	image, err := base64.StdEncoding.DecodeString(response.Data[0].B64JSON)
	if err != nil {
		return nil, "", fmt.Errorf("decode OpenAI image data: %w", err)
	}
	if len(image) == 0 || len(image) > 20<<20 {
		return nil, "", errors.New("OpenAI image response has invalid image size")
	}
	return image, "image/png", nil
}

func imageGenerationPrompt(task Task) string {
	if task.AssetID != "" {
		return "Create safe, non-graphic character design artwork. Show intact characters only. No injury, blood, gore, corpses, dismemberment, torture, or weapon impact.\n\n" + task.Prompt
	}
	return imageSafetyPrompt(task.Prompt)
}

func imageSafetyPrompt(prompt string) string {
	return "Create a PG-13 cinematic still. Show only a tense, non-graphic aftermath or pre-action moment. " +
		"All characters are intact. No injury, blood, gore, corpses, dismemberment, torture, or weapon impact. " +
		"If conflict is implied, use distance, defensive poses, atmospheric light, smoke, silhouettes, or an empty environment.\n\n" + prompt
}

func imageSize(aspectRatio string) string {
	switch aspectRatio {
	case "9:16", "3:4":
		return "1024x1536"
	case "1:1":
		return "1024x1024"
	default:
		return "1536x1024"
	}
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

type MiniMaxProvider struct {
	baseURL string
	apiKey  string
	allowed bool
	client  *http.Client
}

func NewMiniMaxProvider(baseURL, apiKey string, allowed bool) (*MiniMaxProvider, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.minimaxi.com"
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
		client:  &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (p *MiniMaxProvider) Submit(ctx context.Context, task Task) (string, error) {
	if !p.allowed {
		return "", errors.New("paid generation disabled; set ALLOW_PAID_GENERATION=true")
	}
	if p.apiKey == "" {
		return "", errors.New("MINIMAX_API_KEY is not configured")
	}
	content := make([]map[string]any, 0, len(task.VideoInputs)+1)
	content = append(content, map[string]any{"type": "text", "text": task.Prompt})
	for _, input := range task.VideoInputs {
		content = append(content, map[string]any{
			"type": "image_url",
			"image_url": map[string]string{
				"url": "data:" + input.ContentType + ";base64," + base64.StdEncoding.EncodeToString(input.Data),
			},
			"role": input.Role,
		})
	}
	payload := map[string]any{
		"model":      task.Model,
		"content":    content,
		"resolution": task.Resolution,
		"duration":   task.Duration,
		"ratio":      task.AspectRatio,
	}
	var response struct {
		TaskID string `json:"task_id"`
	}
	if err := p.requestJSON(ctx, http.MethodPost, "/v2/video_generation", payload, &response); err != nil {
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
		Task struct {
			Status  string `json:"status"`
			Content struct {
				URL string `json:"url"`
			} `json:"content"`
			Error string `json:"error"`
		} `json:"task"`
	}
	path := "/v2/query/video_generation/" + url.PathEscape(taskID)
	if err := p.requestJSON(ctx, http.MethodGet, path, nil, &response); err != nil {
		return ProviderResult{}, err
	}
	switch strings.ToLower(response.Task.Status) {
	case "queued":
		return ProviderResult{Status: ProviderQueueing}, nil
	case "running":
		return ProviderResult{Status: ProviderProcessing}, nil
	case "succeeded":
		u, err := url.Parse(response.Task.Content.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return ProviderResult{}, errors.New("MiniMax response contains invalid output URL")
		}
		return ProviderResult{Status: ProviderSuccess, DownloadURL: response.Task.Content.URL}, nil
	case "failed", "cancelled":
		return ProviderResult{Status: ProviderFailed}, nil
	default:
		return ProviderResult{}, fmt.Errorf("unknown MiniMax task status %q", response.Task.Status)
	}
}

func isLoopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") ||
		strings.EqualFold(host, "host.docker.internal") ||
		net.ParseIP(host).IsLoopback()
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
