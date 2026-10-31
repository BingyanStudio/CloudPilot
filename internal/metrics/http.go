package metrics

import (
	"net/http"

	echoprometheus "github.com/labstack/echo-prometheus"
	"github.com/labstack/echo/v5"
	"github.com/prometheus/client_golang/prometheus"
)

func HTTPMiddleware(registry prometheus.Registerer) echo.MiddlewareFunc {
	return echoprometheus.NewMiddlewareWithConfig(echoprometheus.MiddlewareConfig{
		Subsystem: "http", Registerer: registry, DoNotUseRequestPathFor404: true,
		LabelFuncs: map[string]echoprometheus.LabelValueFunc{
			// Host and arbitrary methods are client-controlled and must not create unbounded series.
			"host": func(*echo.Context, error) string { return "" },
			"method": func(c *echo.Context, _ error) string {
				switch method := c.Request().Method; method {
				case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete,
					http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch:
					return method
				default:
					return "OTHER"
				}
			},
		},
	})
}
