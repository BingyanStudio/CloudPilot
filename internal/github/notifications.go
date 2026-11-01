package github

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"gorm.io/gorm"
)

// Notifier delivers queued issue acknowledgements independently of agent runs.
type Notifier struct {
	DB           *gorm.DB
	Client       *http.Client
	Encryption   cipher.AEAD
	PollInterval time.Duration
	mu           sync.Mutex
}

// Run retries pending acknowledgements until the service context is cancelled.
func (n *Notifier) Run(ctx context.Context) {
	interval := n.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := n.DeliverPending(ctx); err != nil && ctx.Err() == nil {
			slog.Error("GitHub issue acknowledgement failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// DeliverPending sends due acknowledgements and persists delivery or retry state.
func (n *Notifier) DeliverPending(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	var pending []model.GitHubIssueNotification
	if err := n.DB.WithContext(ctx).Where("delivered_at IS NULL AND available_at <= ?", time.Now().UTC()).Order("id ASC").Limit(20).Find(&pending).Error; err != nil {
		return err
	}
	var failures []error
	for _, notification := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		notification.Attempts++
		if err := n.DB.WithContext(ctx).Model(&notification).Update("attempts", notification.Attempts).Error; err != nil {
			failures = append(failures, err)
			continue
		}
		deliveryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := n.deliver(deliveryCtx, notification)
		cancel()
		now := time.Now().UTC()
		updates := map[string]any{"delivered_at": now, "last_error": ""}
		if err != nil {
			updates = map[string]any{"last_error": err.Error(), "available_at": now.Add(notificationDelay(notification.Attempts))}
			failures = append(failures, fmt.Errorf("event %d: %w", notification.EventID, err))
		}
		if err := n.DB.WithContext(ctx).Model(&notification).Updates(updates).Error; err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (n *Notifier) deliver(ctx context.Context, notification model.GitHubIssueNotification) error {
	token, err := model.ResolveSecret(n.Encryption, "GitHub", notification.Token, notification.TokenSecretRef)
	if err != nil {
		return errors.New("GitHub token is unavailable")
	}
	parts := strings.Split(notification.Repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || notification.IssueNumber < 1 {
		return errors.New("invalid GitHub notification target")
	}
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments", url.PathEscape(parts[0]), url.PathEscape(parts[1]), notification.IssueNumber)
	sum := sha256.Sum256([]byte(notification.TaskURL))
	body := fmt.Sprintf("CloudPilot 已接收此 issue 并创建任务。\n\n[在 WebUI 查看任务](%s)\n\n<!-- cloudpilot-task:%x -->", notification.TaskURL, sum)
	for page := 1; ; page++ {
		response, err := n.request(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=100&page=%d", endpoint, page), token, nil)
		if err != nil {
			return err
		}
		var comments []struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(response, &comments); err != nil {
			return errors.New("GitHub returned invalid comments")
		}
		for _, comment := range comments {
			if strings.TrimSpace(comment.Body) == body {
				return nil
			}
		}
		if len(comments) < 100 {
			break
		}
	}
	data, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return err
	}
	_, err = n.request(ctx, http.MethodPost, endpoint, token, data)
	return err
}

func (n *Notifier) request(ctx context.Context, method, endpoint, token string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "CloudPilot")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := *http.DefaultClient
	if n.Client != nil {
		client = *n.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("GitHub comment request failed")
	}
	defer response.Body.Close()
	expected := http.StatusOK
	if method == http.MethodPost {
		expected = http.StatusCreated
	}
	if response.StatusCode != expected {
		return nil, fmt.Errorf("GitHub comment request returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	if err != nil || len(data) > 8<<20 {
		return nil, errors.New("could not read GitHub comment response")
	}
	return data, nil
}

func notificationDelay(attempt int) time.Duration {
	return time.Duration(1<<min(max(attempt, 1), 9)) * time.Second
}
