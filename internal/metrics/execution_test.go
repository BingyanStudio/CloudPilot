package metrics

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

func TestExecutionMetricsCache(t *testing.T) {
	db, err := model.Open("sqlite", filepath.Join(t.TempDir(), "execution.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	now := time.Now().UTC()
	if err := db.Create(&model.Event{Status: "awaiting_review"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AgentSession{RunID: 1, State: "idle", InputTokens: 12, OutputTokens: 3, TotalTokens: 15, UsageUpdatedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	collector := &executionCollector{db: db}
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	value := func(name, label string) float64 {
		t.Helper()
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, family := range families {
			if family.GetName() != name {
				continue
			}
			for _, metric := range family.Metric {
				if label == "" || (len(metric.Label) == 1 && metric.Label[0].GetValue() == label) {
					return metric.GetGauge().GetValue()
				}
			}
		}
		t.Fatalf("metric %s{%s} missing", name, label)
		return 0
	}
	if value("cloudpilot_events", "awaiting_review") != 1 || value("cloudpilot_agent_sessions", "idle") != 1 || value("cloudpilot_agent_tokens", "total") != 15 || value("cloudpilot_agent_usage_sessions", "") != 1 {
		t.Fatal("execution snapshot does not match stored state")
	}
	if err := db.Model(&model.AgentSession{}).Where("run_id = ?", 1).Update("total_tokens", 25).Error; err != nil {
		t.Fatal(err)
	}
	if value("cloudpilot_agent_tokens", "total") != 15 {
		t.Fatal("scrape bypassed the cache")
	}
	collector.checkedAt = now.Add(-6 * time.Minute)
	if value("cloudpilot_agent_tokens", "total") != 25 || value("cloudpilot_execution_metrics_up", "") != 1 {
		t.Fatal("expired snapshot did not refresh")
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	collector.checkedAt = now.Add(-6 * time.Minute)
	if value("cloudpilot_execution_metrics_up", "") != 0 || value("cloudpilot_agent_tokens", "total") != 25 {
		t.Fatal("failed refresh did not preserve and mark the last snapshot")
	}
}
