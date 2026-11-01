package controller

import (
	"database/sql"
	"net/http"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Event returns a stable task view, including queued events and every execution attempt.
func (a *App) Event(c *echo.Context) error {
	id, err := identifier(c)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	var event model.Event
	runs := make([]model.Run, 0)
	err = a.DB.WithContext(c.Request().Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&event, id).Error; err != nil {
			return err
		}
		return tx.Where("event_id = ?", id).Order("id DESC").Find(&runs).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return persistenceError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"event": event, "runs": runs})
}
