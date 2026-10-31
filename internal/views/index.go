package views

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/BingyanStudio/CloudPilot/internal/controller"
	"github.com/labstack/echo/v5"
)

// Register connects MVC controllers to the API and compiled React application.
func Register(e *echo.Echo, app *controller.App, frontendDir string) {
	e.GET("/health", app.Health)
	e.POST("/hooks/:name", app.Webhook)
	e.POST("/api/auth/login", app.Login, app.SameOrigin)
	api := e.Group("/api", app.SameOrigin, app.Authenticate)
	api.GET("/auth/me", app.Me)
	api.POST("/auth/logout", app.Logout)
	api.GET("/overview", app.Overview)
	api.GET("/resources", app.Resources)
	api.POST("/resources", app.SaveResource)
	api.GET("/resources/:kind/:name", app.Resource)
	api.PUT("/resources/:kind/:name", app.SaveResource)
	api.DELETE("/resources/:kind/:name", app.DeleteResource)
	api.POST("/config/import", app.Import)
	api.GET("/config/export", app.Export)
	api.GET("/events", app.Events)
	api.POST("/events/:id/retry", app.Retry)
	api.POST("/pipelines/:name/trigger", app.Trigger)
	api.GET("/runs", app.Runs)
	api.GET("/runs/:id", app.Run)
	api.GET("/runs/:id/logs", app.Logs)
	api.GET("/runs/:id/timeline", app.Timeline)
	api.POST("/runs/:id/messages", app.SendMessage)
	api.POST("/runs/:id/interrupt", app.Interrupt)
	api.POST("/runs/:id/resume", app.Resume)
	api.POST("/runs/:id/close", app.CloseSession)
	api.POST("/runs/:id/cancel", app.Cancel)
	api.GET("/artifacts", app.Artifacts)
	api.GET("/artifacts/:id/download", app.Download)
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
		if relative != "" && !filepath.IsLocal(relative) {
			return c.NoContent(http.StatusNotFound)
		}
		file := filepath.Join(frontendDir, relative)
		if info, err := os.Stat(file); err == nil && info.Mode().IsRegular() {
			return c.File(file)
		}
		if filepath.Ext(relative) != "" {
			return c.NoContent(http.StatusNotFound)
		}
		if _, err := os.Stat(filepath.Join(frontendDir, "index.html")); err != nil {
			return c.JSON(http.StatusNotFound, map[string]string{"message": "frontend is not built; run pnpm build in frontend"})
		}
		return c.File(filepath.Join(frontendDir, "index.html"))
	})
}
