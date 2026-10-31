package metrics

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BingyanStudio/CloudPilot/internal/model"
)

func TestDatabaseMetrics(t *testing.T) {
	db, err := model.Open("sqlite", filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	handler := Handler(RegisterDatabase(sqlDB))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "cloudpilot_database_up 1") || !strings.Contains(response.Body.String(), "go_goroutines") {
		t.Fatalf("missing metrics: %s", response.Body)
	}
	for _, path := range []string{"/", "/api/auth/login"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("metrics listener exposes %s", path)
		}
	}
}
