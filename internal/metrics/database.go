package metrics

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// RegisterDatabase exports process metrics and a cached database health probe without Redis.
func RegisterDatabase(db *sql.DB) *prometheus.Registry {
	r := prometheus.NewRegistry()
	var mu sync.Mutex
	var lastProbe time.Time
	var up float64
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), collectors.NewBuildInfoCollector(),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "cloudpilot_database_up", Help: "Database connectivity, cached for 30 seconds."}, func() float64 {
			mu.Lock()
			defer mu.Unlock()
			if time.Since(lastProbe) > 30*time.Second {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				up = 0
				if db.PingContext(ctx) == nil {
					up = 1
				}
				lastProbe = time.Now()
			}
			return up
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "cloudpilot_database_connections", Help: "Open database connections."}, func() float64 { return float64(db.Stats().OpenConnections) }),
	)
	return r
}
