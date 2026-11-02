package views

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/BingyanStudio/CloudPilot/frontend"
	"github.com/BingyanStudio/CloudPilot/internal/controller"
	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/labstack/echo/v5"
)

// Register connects MVC controllers to the API and compiled React application.
func Register(e *echo.Echo, app *controller.App) {
	files := echo.MustSubFS(frontend.Dist, "dist")
	e.GET("/health", app.Health)
	e.POST("/hooks/:name", app.Webhook)
	e.POST("/api/auth/login", app.Login, app.SameOrigin)
	e.GET("/api/auth/oidc", app.OIDCStatus)
	e.GET("/api/auth/oidc/login", app.OIDCLogin)
	e.GET("/api/auth/oidc/callback", app.OIDCCallback)
	editor := controller.RequireRole(model.RoleEditor)
	admin := controller.RequireRole(model.RoleAdmin)
	api := e.Group("/api", app.SameOrigin, app.Authenticate)
	api.GET("/auth/me", app.Me)
	api.POST("/auth/logout", app.Logout)
	api.GET("/overview", app.Overview)
	api.GET("/resources", app.Resources)
	api.POST("/resources", app.SaveResource, editor)
	api.GET("/resources/:kind/:name", app.Resource)
	api.PUT("/resources/:kind/:name", app.SaveResource, editor)
	api.DELETE("/resources/:kind/:name", app.DeleteResource, editor)
	api.POST("/config/import", app.Import, editor)
	api.GET("/config/export", app.Export)
	api.GET("/events", app.Events)
	api.GET("/events/:id", app.Event)
	api.POST("/events/:id/retry", app.Retry, editor)
	api.POST("/pipelines/:name/trigger", app.Trigger, editor)
	api.GET("/runs", app.Runs)
	api.GET("/runs/:id", app.Run)
	api.GET("/runs/:id/logs", app.Logs)
	api.GET("/runs/:id/timeline", app.Timeline)
	api.POST("/runs/:id/messages", app.SendMessage, editor)
	api.POST("/runs/:id/interrupt", app.Interrupt, editor)
	api.POST("/runs/:id/resume", app.Resume, editor)
	api.POST("/runs/:id/close", app.CloseSession, editor)
	api.POST("/runs/:id/cancel", app.Cancel, editor)
	api.GET("/artifacts", app.Artifacts)
	api.GET("/artifacts/:id/download", app.Download)
	api.GET("/users", app.Users, admin)
	api.POST("/users", app.CreateUser, admin)
	api.PATCH("/users/:id", app.UpdateUser, admin)
	api.DELETE("/users/:id", app.DeleteUser, admin)
	api.GET("/settings/oidc", app.OIDCSettings, admin)
	api.PUT("/settings/oidc", app.SaveOIDCSettings, admin)
	e.GET("/*", func(c *echo.Context) error {
		requestPath := c.Request().URL.Path
		if requestPath == "/api" || strings.HasPrefix(requestPath, "/api/") || strings.HasPrefix(requestPath, "/hooks/") {
			return c.JSON(http.StatusNotFound, map[string]string{"message": "not found"})
		}
		relative := strings.TrimPrefix(requestPath, "/")
		for segment := range strings.SplitSeq(relative, "/") {
			if strings.HasPrefix(segment, ".") {
				return c.NoContent(http.StatusNotFound)
			}
		}
		if relative != "" && !fs.ValidPath(relative) {
			return c.NoContent(http.StatusNotFound)
		}
		if info, err := fs.Stat(files, relative); err == nil && info.Mode().IsRegular() {
			return c.FileFS(relative, files)
		}
		if path.Ext(relative) != "" {
			return c.NoContent(http.StatusNotFound)
		}
		return c.FileFS("index.html", files)
	})
}
