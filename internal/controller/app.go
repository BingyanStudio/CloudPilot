package controller

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/BingyanStudio/CloudPilot/internal/resource"
	"github.com/labstack/echo/v5"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type App struct {
	DB            *gorm.DB
	Engine        *engine.Engine
	Encryption    cipher.AEAD
	DataDir       string
	SecureCookies bool
	PublicURL     string
	loginMu       sync.Mutex
	logins        map[string]loginWindow
}

type loginWindow struct {
	start time.Time
	count int
}

// Health checks the database connection used by the control plane.
func (a *App) Health(c *echo.Context) error {
	db, err := a.DB.DB()
	ctx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
	defer cancel()
	if err != nil || db.PingContext(ctx) != nil {
		return fail(c, http.StatusServiceUnavailable, "database unavailable")
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// SameOrigin rejects browser mutations from a different origin.
func (a *App) SameOrigin(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		switch c.Request().Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if origin := c.Request().Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Host, c.Request().Host) || u.Path != "" || u.User != nil {
					return fail(c, http.StatusForbidden, "cross-origin request rejected")
				}
			}
		}
		return next(c)
	}
}

// Authenticate resolves an opaque, hashed session cookie to its user.
func (a *App) Authenticate(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		cookie, err := c.Cookie("cp_session")
		if err != nil || len(cookie.Value) != 64 {
			return fail(c, http.StatusUnauthorized, "authentication required")
		}
		var session model.Session
		if err := a.DB.Where("token_hash = ? AND expires_at > ?", hashToken(cookie.Value), time.Now().UTC()).First(&session).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fail(c, http.StatusUnauthorized, "session expired")
			}
			return internal(c, err)
		}
		var user model.User
		if err := a.DB.First(&user, session.UserID).Error; err != nil {
			return fail(c, http.StatusUnauthorized, "authentication required")
		}
		c.Set("cloudpilot.user", user)
		return next(c)
	}
}

// Login creates a session after local password verification.
func (a *App) Login(c *echo.Context) error {
	if !a.allowLogin(c.RealIP()) {
		c.Response().Header().Set("Retry-After", "60")
		return fail(c, http.StatusTooManyRequests, "too many login attempts; retry in one minute")
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeBody(c, &body); err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	var user model.User
	err := a.DB.Where("username = ?", body.Username).First(&user).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return internal(c, err)
	}
	// The fallback hash keeps unknown-user requests on the password-checking path.
	passwordHash := user.PasswordHash
	if err != nil {
		passwordHash = "$2a$10$7EqJtq98hPqEX7fNZaFWoO5uKf9c9bzXBQ9S.LW5RG.Iow.UTIIdW"
	}
	passwordErr := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(body.Password))
	if err != nil || passwordErr != nil {
		return fail(c, http.StatusUnauthorized, "invalid username or password")
	}
	if err := a.startSession(c, user); err != nil {
		return internal(c, err)
	}
	return c.JSON(http.StatusOK, user)
}

// startSession issues a 24-hour opaque session cookie for an authenticated user.
func (a *App) startSession(c *echo.Context, user model.User) error {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return err
	}
	token := hex.EncodeToString(data)
	expires := time.Now().UTC().Add(24 * time.Hour)
	if err := a.DB.Create(&model.Session{TokenHash: hashToken(token), UserID: user.ID, ExpiresAt: expires}).Error; err != nil {
		return err
	}
	a.DB.Where("expires_at <= ?", time.Now().UTC()).Delete(&model.Session{})
	c.SetCookie(&http.Cookie{Name: "cp_session", Value: token, Path: "/", HttpOnly: true, Secure: a.SecureCookies, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: 86400})
	return nil
}

// RequireRole rejects authenticated users whose role ranks below the given role.
func RequireRole(role string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if model.RoleRank(c.Get("cloudpilot.user").(model.User).Role) < model.RoleRank(role) {
				return fail(c, http.StatusForbidden, "insufficient permissions")
			}
			return next(c)
		}
	}
}

// Me returns the authenticated user and role.
func (a *App) Me(c *echo.Context) error {
	return c.JSON(http.StatusOK, c.Get("cloudpilot.user").(model.User))
}

// Logout invalidates the session server-side and removes the browser cookie.
func (a *App) Logout(c *echo.Context) error {
	if cookie, err := c.Cookie("cp_session"); err == nil {
		if err := a.DB.Where("token_hash = ?", hashToken(cookie.Value)).Delete(&model.Session{}).Error; err != nil {
			return internal(c, err)
		}
	}
	c.SetCookie(&http.Cookie{Name: "cp_session", Path: "/", HttpOnly: true, Secure: a.SecureCookies, SameSite: http.SameSiteStrictMode, Expires: time.Unix(1, 0), MaxAge: -1})
	return c.NoContent(http.StatusNoContent)
}

// Resources lists declarative objects, optionally narrowed by kind.
func (a *App) Resources(c *echo.Context) error {
	var items []model.Resource
	query := a.DB.Order("kind ASC, name ASC")
	if kind := c.QueryParam("kind"); kind != "" {
		query = query.Where("kind = ?", kind)
	}
	if err := query.Find(&items).Error; err != nil {
		return internal(c, err)
	}
	docs := make([]model.Document, 0, len(items))
	for _, item := range items {
		docs = append(docs, item.Document())
	}
	return c.JSON(http.StatusOK, docs)
}

// Resource returns one declarative object.
func (a *App) Resource(c *echo.Context) error {
	item, err := model.FindResource(a.DB, c.Param("kind"), c.Param("name"))
	if err != nil {
		return persistenceError(c, err)
	}
	return c.JSON(http.StatusOK, item.Document())
}

// SaveResource applies the same validation and defaults used by YAML imports.
func (a *App) SaveResource(c *echo.Context) error {
	var doc model.Document
	if err := decodeBody(c, &doc); err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	if c.Request().Method == http.MethodPut && (doc.Kind != c.Param("kind") || doc.Metadata.Name != c.Param("name")) {
		return fail(c, http.StatusBadRequest, "URL and resource identity must match")
	}
	result, err := resource.Apply(a.DB, []model.Document{doc}, a.Encryption)
	if err != nil {
		return manifestError(c, err)
	}
	return c.JSON(http.StatusOK, result[0])
}

// DeleteResource removes an unreferenced resource.
func (a *App) DeleteResource(c *echo.Context) error {
	if err := resource.Delete(a.DB, c.Param("kind"), c.Param("name")); err != nil {
		if errors.Is(err, resource.ErrReferenced) {
			return fail(c, http.StatusConflict, err.Error())
		}
		return persistenceError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// Import atomically applies a multi-document YAML configuration.
func (a *App) Import(c *echo.Context) error {
	var body struct {
		YAML string `json:"yaml"`
	}
	if err := decodeBody(c, &body); err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	docs, err := resource.ParseYAML([]byte(body.YAML))
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	result, err := resource.Apply(a.DB, docs, a.Encryption)
	if err != nil {
		return manifestError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

// Export emits all declarative resources as YAML with secret references preserved.
func (a *App) Export(c *echo.Context) error {
	var items []model.Resource
	if err := a.DB.Order("kind ASC, name ASC").Find(&items).Error; err != nil {
		return internal(c, err)
	}
	docs := make([]model.Document, 0, len(items))
	for _, item := range items {
		docs = append(docs, item.Document())
	}
	data, err := resource.ExportYAML(docs)
	if err != nil {
		return internal(c, err)
	}
	c.Response().Header().Set("Content-Disposition", `attachment; filename="cloudpilot-resources.yaml"`)
	return c.Blob(http.StatusOK, "application/yaml", data)
}

// Webhook authenticates an input and durably queues a JSON event.
func (a *App) Webhook(c *echo.Context) error {
	item, err := model.FindResource(a.DB, "Input", c.Param("name"))
	if err != nil {
		return persistenceError(c, err)
	}
	var spec model.InputSpec
	if err := item.Decode(&spec); err != nil {
		return internal(c, err)
	}
	if spec.Paused {
		return fail(c, http.StatusConflict, "input is paused")
	}
	if spec.Type == "github_issue" {
		return a.githubIssue(c, item.Name, spec)
	}
	secret, err := model.ResolveSecret(a.Encryption, "Input", spec.Secret, spec.SecretRef)
	if err != nil {
		return fail(c, http.StatusServiceUnavailable, "input secret is unavailable")
	}
	header := c.Request().Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, "Bearer ")), []byte(secret)) != 1 {
		return fail(c, http.StatusUnauthorized, "invalid webhook credential")
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, 1<<20))
	if err != nil {
		return fail(c, http.StatusRequestEntityTooLarge, "webhook payload must not exceed 1 MiB")
	}
	if !json.Valid(body) {
		return fail(c, http.StatusBadRequest, "webhook payload must be valid JSON")
	}
	if _, err := model.FindResource(a.DB, "Pipeline", spec.PipelineRef); err != nil {
		return fail(c, http.StatusConflict, "input pipeline is unavailable")
	}
	event := model.Event{InputName: item.Name, PipelineName: spec.PipelineRef, Status: "queued", Payload: json.RawMessage(body), AvailableAt: time.Now().UTC()}
	if key := c.Request().Header.Get("Idempotency-Key"); key != "" {
		if len(key) > 190 {
			return fail(c, http.StatusBadRequest, "Idempotency-Key must not exceed 190 bytes")
		}
		event.IdempotencyKey = &key
	}
	query := a.DB
	if event.IdempotencyKey != nil {
		query = query.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "input_name"}, {Name: "idempotency_key"}}, DoNothing: true})
	}
	result := query.Create(&event)
	if result.Error != nil {
		return internal(c, result.Error)
	}
	if event.IdempotencyKey != nil {
		var canonical model.Event
		if err := a.DB.Where("input_name = ? AND idempotency_key = ?", item.Name, *event.IdempotencyKey).First(&canonical).Error; err != nil {
			return internal(c, err)
		}
		event = canonical
	}
	return c.JSON(http.StatusAccepted, event)
}

// Events returns the newest 200 events with optional pipeline and status filters.
func (a *App) Events(c *echo.Context) error {
	items := make([]model.Event, 0)
	if err := filtered(a.DB, c).Order("id DESC").Limit(200).Find(&items).Error; err != nil {
		return internal(c, err)
	}
	return c.JSON(http.StatusOK, items)
}

// Runs returns the newest 200 attempts with optional pipeline and status filters.
func (a *App) Runs(c *echo.Context) error {
	items := make([]model.Run, 0)
	if err := filtered(a.DB, c).Order("id DESC").Limit(200).Find(&items).Error; err != nil {
		return internal(c, err)
	}
	return c.JSON(http.StatusOK, items)
}

// Run combines an execution attempt with its input and collected artifacts.
func (a *App) Run(c *echo.Context) error {
	id, err := identifier(c)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	// Engine reads acquire its mutex before accessing the database; keep that lock order outside the snapshot transaction.
	observedSession, capabilities, err := a.sessionInfo(id)
	if err != nil {
		return persistenceError(c, err)
	}
	var run model.Run
	var event model.Event
	var session *model.AgentSession
	artifacts := make([]model.Artifact, 0)
	turns := make([]model.AgentTurn, 0)
	err = a.DB.WithContext(c.Request().Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&run, id).Error; err != nil {
			return err
		}
		if err := tx.First(&event, run.EventID).Error; err != nil {
			return err
		}
		var stored model.AgentSession
		if err := tx.Where("run_id = ?", id).First(&stored).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		} else {
			session = &stored
		}
		if err := tx.Where("run_id = ?", id).Order("id ASC").Find(&artifacts).Error; err != nil {
			return err
		}
		return tx.Where("run_id = ?", id).Order("number ASC, id ASC").Find(&turns).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return persistenceError(c, err)
	}
	if observedSession == nil || session == nil || observedSession.ID != session.ID || observedSession.State != session.State || observedSession.ThreadID != session.ThreadID || observedSession.CurrentTurnID != session.CurrentTurnID || !observedSession.UpdatedAt.Equal(session.UpdatedAt) {
		// A transition between the live capability check and the snapshot must not advertise stale actions.
		capabilities.CanSend, capabilities.CanInterrupt, capabilities.CanResume, capabilities.CanClose = false, false, false, false
	}
	return c.JSON(http.StatusOK, map[string]any{
		"run": run, "event": event, "artifacts": artifacts,
		"session": session, "capabilities": capabilities, "turns": turns,
	})
}

// Logs supports incremental polling using the last observed log ID.
func (a *App) Logs(c *echo.Context) error {
	id, err := identifier(c)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	var run model.Run
	if err := a.DB.First(&run, id).Error; err != nil {
		return persistenceError(c, err)
	}
	after := uint64(0)
	if value := c.QueryParam("after"); value != "" {
		after, err = strconv.ParseUint(value, 10, 63)
		if err != nil {
			return fail(c, http.StatusBadRequest, "after must be an unsigned log ID")
		}
	}
	items := make([]model.RunLog, 0)
	if err := a.DB.Where("run_id = ? AND id > ?", id, after).Order("id ASC").Limit(1000).Find(&items).Error; err != nil {
		return internal(c, err)
	}
	return c.JSON(http.StatusOK, items)
}

// Cancel stops a running attempt through the scheduler.
func (a *App) Cancel(c *echo.Context) error {
	id, err := identifier(c)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	if a.Engine == nil {
		return fail(c, http.StatusServiceUnavailable, "scheduler unavailable")
	}
	if err := a.Engine.Cancel(id); err != nil {
		return actionError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// Retry queues another attempt of a failed or cancelled event.
func (a *App) Retry(c *echo.Context) error {
	id, err := identifier(c)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	if a.Engine == nil {
		return fail(c, http.StatusServiceUnavailable, "scheduler unavailable")
	}
	event, err := a.Engine.Retry(id)
	if err != nil {
		return actionError(c, err)
	}
	return c.JSON(http.StatusOK, event)
}

// Trigger releases the pipeline's currently queued events for immediate processing.
func (a *App) Trigger(c *echo.Context) error {
	if a.Engine == nil {
		return fail(c, http.StatusServiceUnavailable, "scheduler unavailable")
	}
	if err := a.Engine.Trigger(c.Param("name")); err != nil {
		return actionError(c, err)
	}
	return c.JSON(http.StatusAccepted, map[string]string{"message": "pipeline triggered"})
}

// Artifacts lists the latest collected outputs.
func (a *App) Artifacts(c *echo.Context) error {
	items := make([]model.Artifact, 0)
	if err := a.DB.Order("id DESC").Limit(200).Find(&items).Error; err != nil {
		return internal(c, err)
	}
	return c.JSON(http.StatusOK, items)
}

// Download serves an artifact beneath the configured data directory.
func (a *App) Download(c *echo.Context) error {
	id, err := identifier(c)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	var artifact model.Artifact
	if err := a.DB.First(&artifact, id).Error; err != nil {
		return persistenceError(c, err)
	}
	if artifact.Kind != "file" || artifact.Path == "" {
		return fail(c, http.StatusNotFound, "artifact has no downloadable file")
	}
	base, err := filepath.Abs(a.DataDir)
	if err != nil {
		return internal(c, err)
	}
	filePath := artifact.Path
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(base, filePath)
	}
	rel, err := filepath.Rel(base, filePath)
	if err != nil || !filepath.IsLocal(rel) {
		return fail(c, http.StatusNotFound, "artifact file unavailable")
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return fail(c, http.StatusNotFound, "artifact file unavailable")
	}
	defer root.Close()
	file, err := root.Open(rel)
	if err != nil {
		return fail(c, http.StatusNotFound, "artifact file unavailable")
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return fail(c, http.StatusNotFound, "artifact file unavailable")
	}
	c.Response().Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": artifact.Name}))
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	c.Response().Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	return c.Stream(http.StatusOK, "application/octet-stream", file)
}

// Overview aggregates persisted resource, event and execution counts.
func (a *App) Overview(c *echo.Context) error {
	type countRow struct {
		Name  string
		Count int64
	}
	result := map[string]map[string]int64{"resources": {}, "events": {}, "runs": {}}
	for _, group := range []struct {
		name, column string
		table        any
	}{{"resources", "kind", &model.Resource{}}, {"events", "status", &model.Event{}}, {"runs", "status", &model.Run{}}} {
		var rows []countRow
		if err := a.DB.Model(group.table).Select(group.column + " AS name, COUNT(*) AS count").Group(group.column).Scan(&rows).Error; err != nil {
			return internal(c, err)
		}
		for _, row := range rows {
			result[group.name][row.Name] = row.Count
		}
	}
	return c.JSON(http.StatusOK, result)
}

func (a *App) allowLogin(ip string) bool {
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	now := time.Now().UTC()
	if a.logins == nil {
		a.logins = make(map[string]loginWindow)
	}
	if len(a.logins) > 1000 {
		for key, window := range a.logins {
			if now.Sub(window.start) >= time.Minute {
				delete(a.logins, key)
			}
		}
	}
	window := a.logins[ip]
	if now.Sub(window.start) >= time.Minute {
		window = loginWindow{start: now}
	}
	window.count++
	a.logins[ip] = window
	return window.count <= 10
}

func hashToken(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func decodeBody(c *echo.Context, target any) error {
	data, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, 2<<20))
	if err != nil {
		return errors.New("request body exceeds 2 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON value")
	}
	return nil
}

func identifier(c *echo.Context) (uint, error) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		return 0, errors.New("id must be a positive integer")
	}
	return uint(id), nil
}

func filtered(db *gorm.DB, c *echo.Context) *gorm.DB {
	if pipeline := c.QueryParam("pipeline"); pipeline != "" {
		db = db.Where("pipeline_name = ?", pipeline)
	}
	if status := c.QueryParam("status"); status != "" {
		db = db.Where("status = ?", status)
	}
	return db
}

func fail(c *echo.Context, status int, message string) error {
	return c.JSON(status, map[string]string{"message": message})
}

func internal(c *echo.Context, err error) error {
	c.Logger().Error("request failed", "error", err)
	return fail(c, http.StatusInternalServerError, "internal server error")
}

func persistenceError(c *echo.Context, err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fail(c, http.StatusNotFound, "not found")
	}
	return internal(c, err)
}

func manifestError(c *echo.Context, err error) error {
	if errors.Is(err, resource.ErrConflict) {
		return fail(c, http.StatusConflict, err.Error())
	}
	return fail(c, http.StatusBadRequest, err.Error())
}

func actionError(c *echo.Context, err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fail(c, http.StatusNotFound, "not found")
	}
	return fail(c, http.StatusConflict, err.Error())
}
