package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestNotificationDeliveryRetryAndRecovery(t *testing.T) {
	db, err := model.Open("sqlite", filepath.Join(t.TempDir(), "notifications.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	encryption, err := model.Unlock(db, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	token, err := model.Encrypt(encryption, "GitHub", "notification-secret")
	if err != nil {
		t.Fatal(err)
	}
	notification := model.GitHubIssueNotification{
		EventID: 12, Repository: "org/project", IssueNumber: 42,
		Token: token, TaskURL: "https://pilot.example/#/events/12",
		AvailableAt: time.Now().UTC().Add(-time.Second),
	}
	if err := db.Create(&notification).Error; err != nil {
		t.Fatal(err)
	}
	posts, gets := 0, 0
	postedBody, recovered := "", false
	notifier := &Notifier{DB: db, Encryption: encryption, Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme != "https" || req.URL.Host != "api.github.com" || req.URL.Path != "/repos/org/project/issues/42/comments" {
			t.Fatalf("unexpected GitHub request: %s", req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer notification-secret" || req.Header.Get("X-GitHub-Api-Version") == "" {
			t.Fatal("missing GitHub authentication or API version")
		}
		status, body := http.StatusOK, "[]"
		switch req.Method {
		case http.MethodGet:
			gets++
			if recovered {
				comments := []map[string]string{{"body": postedBody}}
				if req.URL.Query().Get("page") == "1" {
					comments = make([]map[string]string, 100)
					for i := range comments {
						comments[i] = map[string]string{"body": "another comment"}
					}
				}
				data, _ := json.Marshal(comments)
				body = string(data)
			}
		case http.MethodPost:
			posts++
			var comment struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(req.Body).Decode(&comment); err != nil {
				t.Fatal(err)
			}
			postedBody = comment.Body
			if !strings.Contains(comment.Body, "[在 WebUI 查看任务](https://pilot.example/#/events/12)") || !strings.Contains(comment.Body, "<!-- cloudpilot-task:") {
				t.Fatalf("missing task link or recovery marker: %s", comment.Body)
			}
			status, body = http.StatusCreated, `{}`
			if posts == 1 {
				status, body = http.StatusForbidden, `{"message":"notification-secret must not be logged"}`
			}
		default:
			t.Fatalf("unexpected method %s", req.Method)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	ctx := context.Background()
	if err := notifier.DeliverPending(ctx); err == nil || strings.Contains(err.Error(), "notification-secret") {
		t.Fatalf("expected sanitized delivery error, got %v", err)
	}
	if err := db.First(&notification, notification.ID).Error; err != nil {
		t.Fatal(err)
	}
	if notification.DeliveredAt != nil || notification.Attempts != 1 || !notification.AvailableAt.After(time.Now().UTC()) || !strings.Contains(notification.LastError, "403") {
		t.Fatalf("missing retry state: %+v", notification)
	}
	if err := notifier.DeliverPending(ctx); err != nil || posts != 1 || gets != 1 {
		t.Fatalf("backoff did not defer retry: posts=%d gets=%d err=%v", posts, gets, err)
	}
	if err := db.Model(&notification).Update("available_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if err := notifier.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&notification, notification.ID).Error; err != nil {
		t.Fatal(err)
	}
	if notification.DeliveredAt == nil || notification.Attempts != 2 || notification.LastError != "" || posts != 2 {
		t.Fatalf("delivery not persisted: %+v, posts=%d", notification, posts)
	}
	if err := notifier.DeliverPending(ctx); err != nil || posts != 2 || gets != 2 {
		t.Fatalf("delivered notification was repeated: posts=%d gets=%d err=%v", posts, gets, err)
	}
	// Simulate a restart after GitHub accepted the comment but before the receipt was saved.
	recovered = true
	if err := db.Model(&notification).Updates(map[string]any{"delivered_at": nil, "available_at": time.Now().UTC().Add(-time.Second)}).Error; err != nil {
		t.Fatal(err)
	}
	encryption, err = model.Unlock(db, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	notifier = &Notifier{DB: db, Encryption: encryption, Client: notifier.Client}
	if err := notifier.DeliverPending(ctx); err != nil || posts != 2 || gets != 4 {
		t.Fatalf("comment reconciliation failed: posts=%d gets=%d err=%v", posts, gets, err)
	}
	var stored model.GitHubIssueNotification
	if err := db.First(&stored, notification.ID).Error; err != nil || stored.DeliveredAt == nil {
		t.Fatalf("recovered delivery not persisted: %+v, %v", stored, err)
	}
}

func TestNotificationDoesNotFollowRedirects(t *testing.T) {
	requests := 0
	notifier := &Notifier{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if requests != 1 {
			t.Fatal("followed an API redirect with a credential")
		}
		return &http.Response{
			StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://another.example/comments"}},
			Body: io.NopCloser(strings.NewReader("redirect")), Request: req,
		}, nil
	})}}
	_, err := request(context.Background(), notifier.Client, http.MethodGet, "https://api.github.com/repos/org/repo/issues/1/comments", "secret", nil)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(http.StatusFound)) {
		t.Fatalf("redirect was accepted: %v", err)
	}
}
