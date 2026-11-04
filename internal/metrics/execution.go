package metrics

import (
	"context"
	"sync"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"gorm.io/gorm"
)

var (
	eventCount       = prometheus.NewDesc("cloudpilot_events", "Events by status, cached for five minutes.", []string{"status"}, nil)
	sessionCount     = prometheus.NewDesc("cloudpilot_agent_sessions", "Agent sessions by state, cached for five minutes.", []string{"state"}, nil)
	agentTokens      = prometheus.NewDesc("cloudpilot_agent_tokens", "Reported cumulative session tokens, cached for five minutes.", []string{"kind"}, nil)
	usageSessions    = prometheus.NewDesc("cloudpilot_agent_usage_sessions", "Sessions with reported token usage in the cached snapshot.", nil, nil)
	executionUp      = prometheus.NewDesc("cloudpilot_execution_metrics_up", "Whether the last execution metrics refresh succeeded.", nil, nil)
	executionUpdated = prometheus.NewDesc("cloudpilot_execution_metrics_updated_timestamp_seconds", "Unix timestamp of the last successful execution metrics refresh.", nil, nil)
)

type executionCollector struct {
	db                   *gorm.DB
	mu                   sync.Mutex
	checkedAt, updatedAt time.Time
	healthy              bool
	sample               []prometheus.Metric
}

// RegisterExecution adds cached execution statistics to the existing metrics registry.
func RegisterExecution(registry *prometheus.Registry, db *gorm.DB) {
	registry.MustRegister(&executionCollector{db: db})
}

// Describe exposes the collector's fixed metric families.
func (c *executionCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{eventCount, sessionCount, agentTokens, usageSessions, executionUp, executionUpdated} {
		ch <- desc
	}
}

// Collect keeps the last successful snapshot when a bounded refresh fails.
func (c *executionCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	if time.Since(c.checkedAt) >= 5*time.Minute {
		c.checkedAt = time.Now()
		sample, err := c.refresh()
		c.healthy = err == nil
		if err == nil {
			c.sample, c.updatedAt = sample, c.checkedAt
		}
	}
	sample, updatedAt := c.sample, c.updatedAt
	up := 0.0
	if c.healthy {
		up = 1
	}
	c.mu.Unlock()
	for _, metric := range sample {
		ch <- metric
	}
	ch <- prometheus.MustNewConstMetric(executionUp, prometheus.GaugeValue, up)
	if !updatedAt.IsZero() {
		ch <- prometheus.MustNewConstMetric(executionUpdated, prometheus.GaugeValue, float64(updatedAt.Unix()))
	}
}

// refresh reads aggregate counts without adding per-task metric labels.
func (c *executionCollector) refresh() ([]prometheus.Metric, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	db := c.db.WithContext(ctx)
	var sample []prometheus.Metric
	for _, group := range []struct {
		table  any
		column string
		desc   *prometheus.Desc
		states []string
	}{
		{&model.Event{}, "status", eventCount, []string{"queued", "waiting", "running", "awaiting_review", "succeeded", "failed", "interrupted", "cancelled", "unknown"}},
		{&model.AgentSession{}, "state", sessionCount, []string{"starting", "running", "queued", "idle", "interrupting", "interrupted", "failed", "closing", "closed", "unknown"}},
	} {
		var rows []struct {
			State string
			Count int64
		}
		if err := db.Model(group.table).Select(group.column + " AS state, COUNT(*) AS count").Group(group.column).Scan(&rows).Error; err != nil {
			return nil, err
		}
		counts := make(map[string]int64, len(group.states))
		for _, state := range group.states {
			counts[state] = 0
		}
		for _, row := range rows {
			if _, ok := counts[row.State]; !ok {
				row.State = "unknown"
			}
			counts[row.State] += row.Count
		}
		for _, state := range group.states {
			sample = append(sample, prometheus.MustNewConstMetric(group.desc, prometheus.GaugeValue, float64(counts[state]), state))
		}
	}
	var usage struct{ Input, Output, Total, Reported int64 }
	if err := db.Model(&model.AgentSession{}).Where("usage_updated_at IS NOT NULL").Select("COALESCE(SUM(input_tokens), 0) AS input, COALESCE(SUM(output_tokens), 0) AS output, COALESCE(SUM(total_tokens), 0) AS total, COUNT(*) AS reported").Scan(&usage).Error; err != nil {
		return nil, err
	}
	for kind, value := range map[string]int64{"input": usage.Input, "output": usage.Output, "total": usage.Total} {
		sample = append(sample, prometheus.MustNewConstMetric(agentTokens, prometheus.GaugeValue, float64(value), kind))
	}
	return append(sample, prometheus.MustNewConstMetric(usageSessions, prometheus.GaugeValue, float64(usage.Reported))), nil
}
