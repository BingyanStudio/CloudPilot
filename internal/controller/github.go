package controller

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type githubIssuePayload struct {
	Action     string `json:"action"`
	Repository struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"repository"`
	Issue struct {
		ID          int64           `json:"id"`
		Number      int             `json:"number"`
		State       string          `json:"state"`
		PullRequest json.RawMessage `json:"pull_request"`
		Assignees   []struct {
			Login string `json:"login"`
		} `json:"assignees"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"issue"`
}

// githubIssue authenticates GitHub deliveries and queues each matching issue once.
func (a *App) githubIssue(c *echo.Context, inputName string, spec model.InputSpec) error {
	secret := os.Getenv(spec.SecretRef)
	if secret == "" || spec.GitHub == nil {
		return fail(c, http.StatusServiceUnavailable, "GitHub input configuration is unavailable")
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, 1<<20))
	if err != nil {
		return fail(c, http.StatusRequestEntityTooLarge, "webhook payload must not exceed 1 MiB")
	}
	if !validGitHubSignature(body, secret, c.Request().Header.Get("X-Hub-Signature-256")) {
		return fail(c, http.StatusUnauthorized, "invalid webhook signature")
	}
	var payload githubIssuePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fail(c, http.StatusBadRequest, "webhook payload must be a valid GitHub JSON object")
	}
	if c.Request().Header.Get("X-GitHub-Event") != "issues" {
		return c.JSON(http.StatusOK, map[string]string{"status": "ignored"})
	}
	if !slices.Contains([]string{"opened", "reopened", "assigned", "labeled"}, payload.Action) || payload.Issue.State != "open" || len(payload.Issue.PullRequest) != 0 {
		return c.JSON(http.StatusOK, map[string]string{"status": "ignored"})
	}
	if payload.Repository.ID <= 0 || payload.Issue.ID <= 0 || payload.Issue.Number <= 0 {
		return fail(c, http.StatusBadRequest, "repository and issue identifiers are required")
	}
	if !strings.EqualFold(payload.Repository.FullName, spec.GitHub.Repository) || !payload.matches(*spec.GitHub) {
		return c.JSON(http.StatusOK, map[string]string{"status": "ignored"})
	}
	if os.Getenv(spec.GitHub.TokenSecretRef) == "" {
		return fail(c, http.StatusServiceUnavailable, "GitHub token is unavailable")
	}
	if err := validatePublicURL(a.PublicURL); err != nil {
		return fail(c, http.StatusServiceUnavailable, err.Error())
	}
	if _, err := model.FindResource(a.DB, "Pipeline", spec.PipelineRef); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fail(c, http.StatusConflict, "input pipeline is unavailable")
		}
		return internal(c, err)
	}
	var context map[string]json.RawMessage
	if err := json.Unmarshal(body, &context); err != nil {
		return fail(c, http.StatusBadRequest, "webhook payload must be a JSON object")
	}
	context["source"] = json.RawMessage(`"github_issue"`)
	if delivery := c.Request().Header.Get("X-GitHub-Delivery"); delivery != "" {
		context["deliveryId"], _ = json.Marshal(delivery)
	}
	encoded, err := json.Marshal(context)
	if err != nil {
		return internal(c, err)
	}
	key := fmt.Sprintf("github_issue:%d:%d", payload.Repository.ID, payload.Issue.ID)
	event := model.Event{InputName: inputName, PipelineName: spec.PipelineRef, Status: "queued", Payload: encoded, IdempotencyKey: &key, AvailableAt: time.Now().UTC()}
	err = a.DB.WithContext(c.Request().Context()).Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "input_name"}, {Name: "idempotency_key"}}, DoNothing: true}).Create(&event)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return tx.Where("input_name = ? AND idempotency_key = ?", inputName, key).First(&event).Error
		}
		notification := model.GitHubIssueNotification{
			EventID: event.ID, Repository: spec.GitHub.Repository, IssueNumber: payload.Issue.Number,
			TokenSecretRef: spec.GitHub.TokenSecretRef, TaskURL: strings.TrimRight(a.PublicURL, "/") + "/#/events/" + strconv.FormatUint(uint64(event.ID), 10),
			AvailableAt: time.Now().UTC(),
		}
		return tx.Create(&notification).Error
	})
	if err != nil {
		return internal(c, err)
	}
	return c.JSON(http.StatusAccepted, event)
}

// validGitHubSignature checks the SHA-256 signature over the unmodified request body.
func validGitHubSignature(body []byte, secret, signature string) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(provided, mac.Sum(nil))
}

// matches accepts any configured assignee or label, using GitHub's case-insensitive names.
func (payload githubIssuePayload) matches(spec model.GitHubInputSpec) bool {
	for _, expected := range spec.Assignees {
		for _, assignee := range payload.Issue.Assignees {
			if expected == "*" || strings.EqualFold(expected, assignee.Login) {
				return true
			}
		}
	}
	for _, expected := range spec.Labels {
		for _, label := range payload.Issue.Labels {
			if strings.EqualFold(expected, label.Name) {
				return true
			}
		}
	}
	return false
}

// validatePublicURL keeps task links tied to the configured WebUI address.
func validatePublicURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") {
		return errors.New("server.public_url must be configured as an absolute WebUI URL")
	}
	return nil
}
