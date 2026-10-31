package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/extra/redisprometheus/v9"
	"github.com/redis/go-redis/v9"
)

func Register(client *redis.Client) *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewBuildInfoCollector(),
		redisprometheus.NewCollector("", "redis", client),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "redis_up",
			Help: "Whether the application's Redis client can successfully PING Redis.",
		}, func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if client.Ping(ctx).Err() != nil {
				return 0
			}
			return 1
		}),
	)
	return registry
}

func Handler(registry *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.InstrumentMetricHandler(registry, promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		Timeout: 5 * time.Second, MaxRequestsInFlight: 2,
	})))
	return mux
}
