package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Timeline returns normalized session events after the last observed event ID.
func (a *App) Timeline(c *echo.Context) error {
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
			return fail(c, http.StatusBadRequest, "after must be a nonnegative event ID")
		}
	}
	events := make([]model.SessionEvent, 0)
	if err := a.DB.Where("run_id = ? AND id > ?", id, after).Order("id ASC").Limit(1000).Find(&events).Error; err != nil {
		return internal(c, err)
	}
	return c.JSON(http.StatusOK, events)
}

// SendMessage steers an active turn or queues another turn in an idle session.
func (a *App) SendMessage(c *echo.Context) error {
	id, err := identifier(c)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	message, err := sessionMessage(c, false)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	if a.Engine == nil {
		return fail(c, http.StatusServiceUnavailable, "scheduler unavailable")
	}
	user := c.Get("cloudpilot.user").(model.User)
	if err := a.Engine.SendMessage(id, message, user.ID); err != nil {
		return actionError(c, err)
	}
	return c.JSON(http.StatusAccepted, map[string]string{"message": "message accepted"})
}

// Interrupt stops the current agent turn while preserving its session and working files.
func (a *App) Interrupt(c *echo.Context) error {
	id, err := identifier(c)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	if a.Engine == nil {
		return fail(c, http.StatusServiceUnavailable, "scheduler unavailable")
	}
	if err := a.Engine.Interrupt(id); err != nil {
		return actionError(c, err)
	}
	return c.JSON(http.StatusAccepted, map[string]string{"message": "interrupt requested"})
}

// Resume queues a new turn using the existing conversation and working files.
func (a *App) Resume(c *echo.Context) error {
	id, err := identifier(c)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	message, err := sessionMessage(c, true)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	if a.Engine == nil {
		return fail(c, http.StatusServiceUnavailable, "scheduler unavailable")
	}
	user := c.Get("cloudpilot.user").(model.User)
	if err := a.Engine.Resume(id, message, user.ID); err != nil {
		return actionError(c, err)
	}
	return c.JSON(http.StatusAccepted, map[string]string{"message": "continuation queued"})
}

// CloseSession ends a conversation and requests cleanup of its retained runtime storage.
func (a *App) CloseSession(c *echo.Context) error {
	id, err := identifier(c)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	if a.Engine == nil {
		return fail(c, http.StatusServiceUnavailable, "scheduler unavailable")
	}
	if err := a.Engine.CloseSession(id); err != nil {
		return actionError(c, err)
	}
	return c.JSON(http.StatusAccepted, map[string]string{"message": "session close requested"})
}

func (a *App) sessionInfo(runID uint) (*model.AgentSession, engine.Capabilities, error) {
	if a.Engine != nil {
		return a.Engine.SessionInfo(runID)
	}
	var session model.AgentSession
	if err := a.DB.Where("run_id = ?", runID).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, engine.Capabilities{}, nil
		}
		return nil, engine.Capabilities{}, err
	}
	return &session, engine.Capabilities{}, nil
}

func sessionMessage(c *echo.Context, optional bool) (string, error) {
	data, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, 256<<10))
	if err != nil {
		return "", errors.New("message request exceeds 256 KiB")
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 && optional {
		return "", nil
	}
	if len(data) == 0 || data[0] != '{' {
		return "", errors.New("expected a JSON object with a message field")
	}
	var body struct {
		Message string `json:"message"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return "", errors.New("invalid message request")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return "", errors.New("expected one JSON value")
	}
	if len(body.Message) > 32<<10 {
		return "", errors.New("message must not exceed 32 KiB")
	}
	if strings.ContainsRune(body.Message, '\x00') {
		return "", errors.New("message must not contain NUL characters")
	}
	if strings.TrimSpace(body.Message) == "" {
		if !optional {
			return "", errors.New("message must not be empty")
		}
		return "", nil
	}
	return body.Message, nil
}
