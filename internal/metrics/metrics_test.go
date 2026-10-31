package metrics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type pingHook func(context.Context, redis.Cmder) error

func (h pingHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h pingHook) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return redis.ProcessHook(h)
}
func (h pingHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestMetricsRedisHealth(t *testing.T) {
	for _, state := range []string{"up", "down", "timeout"} {
		t.Run(state, func(t *testing.T) {
			client := redis.NewClient(&redis.Options{ContextTimeoutEnabled: true})
			t.Cleanup(func() { client.Close() })
			client.AddHook(pingHook(func(ctx context.Context, cmd redis.Cmder) error {
				deadline, ok := ctx.Deadline()
				if cmd.Name() != "ping" || !ok || time.Until(deadline) > time.Second {
					t.Error("health probe must use PING with a one-second deadline")
				}
				switch state {
				case "down":
					return errors.New("Redis unavailable")
				case "timeout":
					<-ctx.Done()
					return ctx.Err()
				default:
					return nil
				}
			}))
			handler := Handler(Register(client))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("scrape returned %d: %s", response.Code, response.Body)
			}
			up := "0"
			if state == "up" {
				up = "1"
			}
			for _, metric := range []string{"go_goroutines ", "go_memstats_alloc_bytes ", "go_build_info{", "redis_pool_hit_total ", "redis_pool_conn_total_current ", "redis_up " + up + "\n"} {
				if !strings.Contains(response.Body.String(), metric) {
					t.Errorf("missing metric %q", metric)
				}
			}
			for _, request := range []struct {
				method, path string
				status       int
			}{
				{http.MethodGet, "/", http.StatusNotFound},
				{http.MethodGet, "/auth/login", http.StatusNotFound},
				{http.MethodPost, "/metrics", http.StatusMethodNotAllowed},
			} {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(request.method, request.path, nil))
				if response.Code != request.status {
					t.Errorf("%s %s returned %d, want %d", request.method, request.path, response.Code, request.status)
				}
			}
		})
	}
}
