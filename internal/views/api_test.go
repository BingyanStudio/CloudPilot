package views_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/controller"
	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/BingyanStudio/CloudPilot/internal/resource"
	"github.com/BingyanStudio/CloudPilot/internal/views"
	"github.com/labstack/echo/v5"
)

func TestAuthenticatedConfigurationAndWebhookLifecycle(t *testing.T) {
	previousLocation := time.Local
	time.Local = time.FixedZone("Asia/Shanghai", 8*60*60)
	t.Cleanup(func() { time.Local = previousLocation })
	dir := t.TempDir()
	db, err := model.Open("sqlite", filepath.Join(dir, "cloudpilot.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err = model.BootstrapAdmin(db, "admin", "test-password-123"); err != nil {
		t.Fatal(err)
	}
	app := &controller.App{DB: db, DataDir: dir}
	e := echo.New()
	views.Register(e, app, filepath.Join(dir, "frontend"))
	request := func(method, path, body string, cookie *http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, req)
		return recorder
	}
	if got := request("GET", "/api/resources", "", nil, nil).Code; got != 401 {
		t.Fatalf("unauthenticated resources status %d", got)
	}
	if got := request("POST", "/api/auth/login", `{"username":"admin","password":"wrong"}`, nil, nil).Code; got != 401 {
		t.Fatalf("wrong password status %d", got)
	}
	login := request("POST", "/api/auth/login", `{"username":"admin","password":"test-password-123"}`, nil, nil)
	if login.Code != 200 {
		t.Fatalf("login %d %s", login.Code, login.Body)
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("invalid session cookie: %+v", cookies)
	}
	cookie := cookies[0]
	if got := request("GET", "/api/auth/me", "", cookie, nil).Code; got != 200 {
		t.Fatalf("me status %d", got)
	}
	if got := request("POST", "/api/config/import", `{"yaml":""}`, cookie, map[string]string{"Origin": "https://attacker.test"}).Code; got != 403 {
		t.Fatalf("cross-origin mutation status %d", got)
	}
	manifest := `apiVersion: cloudpilot.dev/v1alpha1
kind: Worker
metadata: {name: codex}
spec: {model: gpt-5, apiKeySecretRef: MODEL_KEY}
---
apiVersion: cloudpilot.dev/v1alpha1
kind: Workspace
metadata: {name: app}
spec: {}
---
apiVersion: cloudpilot.dev/v1alpha1
kind: Runner
metadata: {name: docker}
spec: {}
---
apiVersion: cloudpilot.dev/v1alpha1
kind: Pipeline
metadata: {name: nightly}
spec: {workerRef: codex, workspaceRef: app, runnerRef: docker, schedule: "0 20 * * *"}
---
apiVersion: cloudpilot.dev/v1alpha1
kind: Input
metadata: {name: feedback}
spec: {pipelineRef: nightly, secretRef: FEEDBACK_TOKEN}
`
	encoded, _ := json.Marshal(map[string]string{"yaml": manifest})
	response := request("POST", "/api/config/import", string(encoded), cookie, nil)
	if response.Code != 200 {
		t.Fatalf("import %d %s", response.Code, response.Body)
	}
	if got := request("DELETE", "/api/resources/Worker/codex", "", cookie, nil).Code; got != 409 {
		t.Fatalf("referenced delete status %d", got)
	}
	t.Setenv("FEEDBACK_TOKEN", "private-webhook-token")
	t.Setenv("MODEL_KEY", "private-model-key")
	if got := request("POST", "/hooks/feedback", `{"feedback":"crash"}`, nil, nil).Code; got != 401 {
		t.Fatalf("unsigned webhook status %d", got)
	}
	headers := map[string]string{"Authorization": "Bearer private-webhook-token", "Idempotency-Key": "feedback-123"}
	if got := request("POST", "/hooks/feedback", `not json`, nil, headers).Code; got != 400 {
		t.Fatalf("invalid webhook status %d", got)
	}
	first := request("POST", "/hooks/feedback", `{"feedback":"crash"}`, nil, headers)
	if first.Code != 202 {
		t.Fatalf("webhook %d %s", first.Code, first.Body)
	}
	duplicate := request("POST", "/hooks/feedback", `{"feedback":"changed"}`, nil, headers)
	if duplicate.Code != 202 {
		t.Fatalf("duplicate webhook %d %s", duplicate.Code, duplicate.Body)
	}
	var original, again model.Event
	if err = json.Unmarshal(first.Body.Bytes(), &original); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(duplicate.Body.Bytes(), &again); err != nil {
		t.Fatal(err)
	}
	if original.ID != again.ID || !bytes.Equal(original.Payload, again.Payload) {
		t.Fatal("duplicate delivery changed the original event")
	}
	var count int64
	db.Model(&model.Event{}).Count(&count)
	if count != 1 {
		t.Fatalf("duplicate delivery created %d events", count)
	}
	var eligible int64
	if err = db.Model(&model.Event{}).Where("id = ? AND status = ? AND available_at <= ? AND created_at <= ?", original.ID, "queued", time.Now().UTC(), time.Now().UTC()).Count(&eligible).Error; err != nil {
		t.Fatal(err)
	}
	if eligible != 1 {
		t.Fatal("feedback created on a UTC+8 host must be immediately eligible for the UTC scheduler")
	}
	if _, offset := original.AvailableAt.Zone(); offset != 0 {
		t.Fatal("feedback availability must persist as UTC")
	}
	export := request("GET", "/api/config/export", "", cookie, nil)
	if export.Code != 200 || strings.Contains(export.Body.String(), "private-") {
		t.Fatalf("unsafe export %s", export.Body)
	}
	if _, err = resource.ParseYAML(export.Body.Bytes()); err != nil {
		t.Fatalf("invalid exported YAML: %v", err)
	}
	if got := request("GET", "/api/overview", "", cookie, nil).Code; got != 200 {
		t.Fatalf("overview status %d", got)
	}
	legacy := model.Run{EventID: original.ID, PipelineName: original.PipelineName, Status: "succeeded", StartedAt: time.Now().UTC()}
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	response = request("GET", fmt.Sprintf("/api/runs/%d", legacy.ID), "", cookie, nil)
	var detail struct {
		Session      *model.AgentSession `json:"session"`
		Capabilities map[string]bool     `json:"capabilities"`
		Turns        []model.AgentTurn   `json:"turns"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &detail) != nil || detail.Session != nil || detail.Turns == nil || len(detail.Turns) != 0 || len(detail.Capabilities) != 5 {
		t.Fatalf("legacy run lacks safe session defaults: %d %s", response.Code, response.Body)
	}
	for _, enabled := range detail.Capabilities {
		if enabled {
			t.Fatal("legacy run advertised interactive actions")
		}
	}
	if got := request("POST", "/api/auth/logout", "", cookie, nil).Code; got != 204 {
		t.Fatalf("logout status %d", got)
	}
	if got := request("GET", "/api/auth/me", "", cookie, nil).Code; got != 401 {
		t.Fatalf("invalidated cookie status %d", got)
	}
}

func TestArtifactDownloadStaysWithinDataDirectory(t *testing.T) {
	dir := t.TempDir()
	db, err := model.Open("sqlite", filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	app := &controller.App{DB: db, DataDir: dir}
	e := echo.New()
	e.GET("/files/:id", app.Download)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err = os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(dir, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(dir, "build.apk")
	if err = os.WriteFile(inside, []byte("apk contents"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path   string
		status int
	}{{outside, 404}, {filepath.Join(dir, "escape.txt"), 404}, {inside, 200}} {
		artifact := model.Artifact{Kind: "file", Name: "build.apk", Path: tc.path}
		if err = db.Create(&artifact).Error; err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/files/%d", artifact.ID), nil)
		response := httptest.NewRecorder()
		e.ServeHTTP(response, req)
		if response.Code != tc.status {
			t.Errorf("download %s status %d, expected %d", tc.path, response.Code, tc.status)
		}
	}
}
