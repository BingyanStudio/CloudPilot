package views

import (
	"github.com/labstack/echo/v5"
)

func InitViews(e *echo.Echo) {
	HealthCheckRoutes(e.Group("/health"))
}

func HealthCheckRoutes(g *echo.Group) {
	g.GET("/", func(c *echo.Context) error {
		return c.JSON(200, map[string]string{
			"status": "ok",
		})
	})
}
