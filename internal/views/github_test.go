package views_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BingyanStudio/CloudPilot/internal/controller"
	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/BingyanStudio/CloudPilot/internal/views"
	"github.com/labstack/echo/v5"
)

func TestGitHubIssueInput(t *testing.T) {
	t.Setenv("GITHUB_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("GITHUB_COMMENT_TOKEN", "comment-token")
	db, err := model.Open("sqlite", filepath.Join(t.TempDir(), "github.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	for _, item := range []model.Resource{
		{Kind: "Pipeline", Name: "issues", Spec: json.RawMessage(`{}`)},
		{Kind: "Input", Name: "github", Spec: json.RawMessage(`{"type":"github_issue","pipelineRef":"issues","secretRef":"GITHUB_WEBHOOK_SECRET","github":{"repository":"org/repo","tokenSecretRef":"GITHUB_COMMENT_TOKEN","assignees":["agent-bot"],"labels":["agent-ready"]}}`)},
		{Kind: "Input", Name: "any-assignee", Spec: json.RawMessage(`{"type":"github_issue","pipelineRef":"issues","secretRef":"GITHUB_WEBHOOK_SECRET","github":{"repository":"org/repo","tokenSecretRef":"GITHUB_COMMENT_TOKEN","assignees":["*"]}}`)},
	} {
		if err := db.Create(&item).Error; err != nil {
			t.Fatal(err)
		}
	}
	app := &controller.App{DB: db, PublicURL: "https://pilot.example.com/"}
	e := echo.New()
	views.Register(e, app, t.TempDir())
	request := func(input, body, eventType, signature string, delivery ...string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/hooks/"+input, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GitHub-Event", eventType)
		req.Header.Set("X-GitHub-Delivery", "delivery-123")
		if len(delivery) > 0 {
			req.Header.Set("X-GitHub-Delivery", delivery[0])
		}
		req.Header.Set("X-Hub-Signature-256", signature)
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, req)
		return recorder
	}
	fixture := func(id int, action, state, repository, labels, assignees string) string {
		return fmt.Sprintf(`{"action":%q,"repository":{"id":77,"full_name":%q,"html_url":"https://github.com/org/repo"},"issue":{"id":%d,"number":123,"state":%q,"title":"Fix the race","body":"Description and acceptance criteria","html_url":"https://github.com/org/repo/issues/123","labels":%s,"assignees":%s,"user":{"login":"reporter"}},"sender":{"login":"operator"}}`, action, repository, id, state, labels, assignees)
	}
	for _, tc := range []struct {
		name, action, state, repository, labels, assignees, eventType, input string
		want                                                                 int
	}{
		{"label matches", "labeled", "open", "org/repo", `[{"name":"agent-ready"}]`, `[]`, "issues", "github", 202},
		{"assignee matches without label", "assigned", "open", "ORG/REPO", `[]`, `[{"login":"AGENT-BOT"}]`, "issues", "github", 202},
		{"opened matching issue", "opened", "open", "org/repo", `[{"name":"AGENT-READY"}]`, `[]`, "issues", "github", 202},
		{"reopened matching issue", "reopened", "open", "org/repo", `[{"name":"agent-ready"}]`, `[]`, "issues", "github", 202},
		{"wildcard assignee", "assigned", "open", "org/repo", `[]`, `[{"login":"developer"}]`, "issues", "any-assignee", 202},
		{"wildcard unassigned", "opened", "open", "org/repo", `[]`, `[]`, "issues", "any-assignee", 200},
		{"unmatched filters", "opened", "open", "org/repo", `[{"name":"bug"}]`, `[{"login":"someone"}]`, "issues", "github", 200},
		{"other repository", "opened", "open", "org/other", `[{"name":"agent-ready"}]`, `[]`, "issues", "github", 200},
		{"edited issue", "edited", "open", "org/repo", `[{"name":"agent-ready"}]`, `[]`, "issues", "github", 200},
		{"closed issue", "labeled", "closed", "org/repo", `[{"name":"agent-ready"}]`, `[]`, "issues", "github", 200},
		{"issue comment", "opened", "open", "org/repo", `[{"name":"agent-ready"}]`, `[]`, "issue_comment", "github", 200},
		{"pull request event", "opened", "open", "org/repo", `[{"name":"agent-ready"}]`, `[]`, "pull_request", "github", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before int64
			db.Model(&model.Event{}).Count(&before)
			body := fixture(100+int(before), tc.action, tc.state, tc.repository, tc.labels, tc.assignees)
			response := request(tc.input, body, tc.eventType, githubSignature(body))
			if response.Code != tc.want {
				t.Fatalf("status %d, body %s", response.Code, response.Body)
			}
			var after int64
			db.Model(&model.Event{}).Count(&after)
			want := before
			if tc.want == 202 {
				want++
			}
			if after != want {
				t.Fatalf("event count %d, want %d", after, want)
			}
		})
	}
	body := fixture(500, "assigned", "open", "org/repo", `[]`, `[{"login":"agent-bot"}]`)
	for _, signature := range []string{"", "sha256=00", githubSignature(body + " ")} {
		if response := request("github", body, "issues", signature); response.Code != 401 {
			t.Fatalf("invalid signature: %d %s", response.Code, response.Body)
		}
	}
	if response := request("github", "{", "issues", githubSignature("{")); response.Code != 400 {
		t.Fatalf("invalid JSON: %d %s", response.Code, response.Body)
	}
	large := strings.Repeat(" ", (1<<20)+1)
	if response := request("github", large, "issues", githubSignature(large)); response.Code != 413 {
		t.Fatalf("oversized body: %d", response.Code)
	}
	if response := request("github", `{}`, "ping", githubSignature(`{}`)); response.Code != 200 {
		t.Fatalf("ping: %d %s", response.Code, response.Body)
	}
	prBody := strings.Replace(body, `"number":123`, `"number":123,"pull_request":{"url":"https://api.github.com/repos/org/repo/pulls/123"}`, 1)
	if response := request("github", prBody, "issues", githubSignature(prBody)); response.Code != 200 {
		t.Fatalf("issue containing pull request: %d", response.Code)
	}
	for _, unavailable := range []string{"public URL", "token"} {
		t.Run(unavailable, func(t *testing.T) {
			if unavailable == "public URL" {
				app.PublicURL = ""
				defer func() { app.PublicURL = "https://pilot.example.com/" }()
			} else {
				t.Setenv("GITHUB_COMMENT_TOKEN", "")
			}
			if response := request("github", body, "issues", githubSignature(body)); response.Code != 503 {
				t.Fatalf("missing configuration: %d %s", response.Code, response.Body)
			}
		})
	}
	response := request("github", body, "issues", githubSignature(body))
	if response.Code != 202 {
		t.Fatalf("accepted issue: %d %s", response.Code, response.Body)
	}
	var event model.Event
	if err := json.Unmarshal(response.Body.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Source     string `json:"source"`
		DeliveryID string `json:"deliveryId"`
		Issue      struct {
			ID     int    `json:"id"`
			Number int    `json:"number"`
			Title  string `json:"title"`
			Body   string `json:"body"`
		} `json:"issue"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if event.Status != "queued" || payload.Source != "github_issue" || payload.DeliveryID != "delivery-123" || payload.Issue.ID != 500 || payload.Issue.Number != 123 || payload.Issue.Title != "Fix the race" || payload.Issue.Body != "Description and acceptance criteria" || payload.Repository.FullName != "org/repo" {
		t.Fatalf("lost issue context: %s", event.Payload)
	}
	// The same issue may produce different deliveries or qualifying actions.
	repeated := strings.ReplaceAll(body, `"assigned"`, `"opened"`)
	response = request("github", repeated, "issues", githubSignature(repeated), "delivery-456")
	var duplicate model.Event
	if err := json.Unmarshal(response.Body.Bytes(), &duplicate); response.Code != 202 || err != nil || duplicate.ID != event.ID {
		t.Fatalf("duplicate delivery: %d %s", response.Code, response.Body)
	}
	var notification model.GitHubIssueNotification
	if err := db.Where("event_id = ?", event.ID).First(&notification).Error; err != nil {
		t.Fatal(err)
	}
	wantURL := fmt.Sprintf("https://pilot.example.com/#/events/%d", event.ID)
	if notification.Repository != "org/repo" || notification.IssueNumber != 123 || notification.TaskURL != wantURL || notification.TokenSecretRef != "GITHUB_COMMENT_TOKEN" {
		t.Fatalf("notification: %+v", notification)
	}
	var count int64
	db.Model(&model.GitHubIssueNotification{}).Where("event_id = ?", event.ID).Count(&count)
	if count != 1 {
		t.Fatalf("duplicate notifications: %d", count)
	}
}

func githubSignature(body string) string {
	mac := hmac.New(sha256.New, []byte("webhook-secret"))
	_, _ = mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
