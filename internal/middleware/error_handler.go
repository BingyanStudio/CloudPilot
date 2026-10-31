package middleware

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

func CustomHTTPErrorHandler(c *echo.Context, err error) {
	if resp, uErr := echo.UnwrapResponse(c.Response()); uErr == nil {
		if resp.Committed {
			return
		}
	}

	msg := "服务器内部错误"

	code := echo.StatusCode(err)
	if code == 0 {
		// If the error doesn't implement HTTPStatusCoder, default to 500
		code = http.StatusInternalServerError
	}

	// 根据请求方法返回不同的响应
	if c.Request().Method == http.MethodHead {
		c.NoContent(code)
	} else {
		c.JSON(code, map[string]any{
			"code":    code,
			"message": msg,
		})
	}
}
