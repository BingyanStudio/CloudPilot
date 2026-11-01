package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/BingyanStudio/CloudPilot/internal/sandbox"
)

type e2bRoundTrip func(*http.Request) (*http.Response, error)

func (f e2bRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type sandboxControl struct {
	mu             sync.Mutex
	items          map[string]sandbox.Sandbox
	requests       []string
	failCreate     bool
	deleteNotFound bool
}

func (f *sandboxControl) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("X-API-KEY") != "test-sandbox-key" {
		http.Error(w, "missing control credential", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/sandboxes" {
		var body struct {
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sb := sandbox.Sandbox{ID: "created-before-response-failed", State: "running", Metadata: body.Metadata}
		f.items[sb.ID] = sb
		if f.failCreate {
			http.Error(w, "response lost after provisioning", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(sb)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v2/sandboxes" {
		filter, _ := url.ParseQuery(r.URL.Query().Get("metadata"))
		items := []sandbox.Sandbox{}
		for _, sb := range f.items {
			match := true
			for key := range filter {
				match = match && sb.Metadata[key] == filter.Get(key)
			}
			if match {
				items = append(items, sb)
			}
		}
		_ = json.NewEncoder(w).Encode(items)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/sandboxes/")
	sb, exists := f.items[id]
	if !exists {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(sb)
	case http.MethodDelete:
		delete(f.items, id)
		if f.deleteNotFound {
			http.Error(w, "already removed", http.StatusNotFound)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		http.Error(w, "unexpected lifecycle request", http.StatusMethodNotAllowed)
	}
}

func e2bFixture(t *testing.T) (*E2B, model.RunnerSpec, *sandboxControl) {
	t.Helper()
	dir := t.TempDir()
	db, err := model.Open("sqlite", filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	t.Setenv("TEST_E2B_KEY", "test-sandbox-key")
	control := &sandboxControl{items: map[string]sandbox.Sandbox{}}
	server := httptest.NewServer(http.HandlerFunc(control.serve))
	t.Cleanup(server.Close)
	address, _ := url.Parse(server.URL)
	httpClient := &http.Client{Transport: e2bRoundTrip(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		copy.URL.Scheme, copy.URL.Host = address.Scheme, address.Host
		return server.Client().Transport.RoundTrip(copy)
	})}
	executor := &E2B{DB: db, DataDir: dir, HTTPClient: httpClient}
	if _, err := executor.deployment(); err != nil {
		t.Fatal(err)
	}
	runner := model.RunnerSpec{Type: "e2b", E2B: &model.E2BRunnerSpec{Provider: "novita", Domain: "sandbox.test", APIKeySecretRef: "TEST_E2B_KEY", Template: "test-template", LeaseSeconds: 900, IdleStrategy: "pause"}}
	return executor, runner, control
}

func e2bRecord(t *testing.T, executor *E2B, runner model.RunnerSpec, runID uint, state, sandboxID string) model.RuntimeInstance {
	t.Helper()
	deployment, err := executor.deployment()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(runner)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	return model.RuntimeInstance{DeploymentID: deployment, EventID: 10, RunID: runID, AttemptID: fmt.Sprintf("attempt-%d", runID), RunnerSnapshot: snapshot, SandboxID: sandboxID, State: state, ExpiresAt: &expires}
}

func remoteSandbox(record model.RuntimeInstance, state string) sandbox.Sandbox {
	return sandbox.Sandbox{ID: record.SandboxID, State: state, Metadata: map[string]string{"cloudpilot_instance": record.DeploymentID, "cloudpilot_attempt": record.AttemptID}}
}

func TestE2BFindsUncertainCreationBeforeRetry(t *testing.T) {
	executor, runner, control := e2bFixture(t)
	control.failCreate = true
	client, err := executor.client(runner)
	if err != nil {
		t.Fatal(err)
	}
	task := engine.Task{Event: model.Event{ID: 10}, Run: model.Run{ID: 20}, Runner: runner}
	record, _, err := executor.provision(t.Context(), client, task, 0)
	if err == nil || record == nil || record.State != "creating" || record.SandboxID != "" {
		t.Fatalf("expected a durable uncertain create, got record=%+v err=%v", record, err)
	}
	task.Run.ID++
	if err := executor.clearPrevious(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if err := executor.DB.First(record, record.ID).Error; err != nil {
		t.Fatal(err)
	}
	if record.State != "closed" || record.SandboxID != "created-before-response-failed" || record.ClosedAt == nil {
		t.Fatalf("orphan was not found and released before retry: %+v", record)
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if len(control.items) != 0 || !slices.Contains(control.requests, "DELETE /sandboxes/created-before-response-failed") {
		t.Fatalf("orphan survived retry cleanup: %v", control.requests)
	}
}

func TestE2BUncertainCreationBlocksRetryUntilLeaseExpires(t *testing.T) {
	executor, runner, control := e2bFixture(t)
	record := e2bRecord(t, executor, runner, 20, "creating", "")
	if err := executor.DB.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	task := engine.Task{Event: model.Event{ID: record.EventID}, Run: model.Run{ID: 21}, Runner: runner}
	if err := executor.clearPrevious(t.Context(), task); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("retry was not blocked while an unseen create may still complete: %v", err)
	}
	if err := executor.DB.First(&record, record.ID).Error; err != nil || record.State != "creating" {
		t.Fatalf("uncertain resource was discarded: %+v %v", record, err)
	}
	if err := executor.DB.Model(&record).Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if err := executor.clearPrevious(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if err := executor.DB.First(&record, record.ID).Error; err != nil || record.State != "closed" {
		t.Fatalf("expired missing resource was not released: %+v %v", record, err)
	}
	retained := e2bRecord(t, executor, runner, 22, "creating", "")
	retained.SessionID = 99
	expired := time.Now().Add(-time.Hour)
	retained.ExpiresAt = &expired
	if err := executor.DB.Create(&retained).Error; err != nil {
		t.Fatal(err)
	}
	client, err := executor.client(runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.resolvePending(t.Context(), client, &retained); err == nil || retained.State != "creating" {
		t.Fatalf("auto-paused workspace was assumed deleted after lease expiry: %+v %v", retained, err)
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	for _, request := range control.requests {
		if request != "GET /v2/sandboxes" {
			t.Fatalf("cleanup created or modified an unconfirmed sandbox: %s", request)
		}
	}
}

func TestE2BRecoveryRetainsSessionsAndDetectsLostWorkspaces(t *testing.T) {
	executor, runner, control := e2bFixture(t)
	batch := e2bRecord(t, executor, runner, 20, "running", "owned-batch")
	retained := e2bRecord(t, executor, runner, 21, "paused", "owned-session")
	missing := e2bRecord(t, executor, runner, 22, "paused", "missing-session")
	sessions := []model.AgentSession{{RunID: retained.RunID, ThreadID: "thread-retained", State: "idle"}, {RunID: missing.RunID, ThreadID: "thread-missing", State: "idle"}}
	if err := executor.DB.Create(&sessions).Error; err != nil {
		t.Fatal(err)
	}
	retained.SessionID, missing.SessionID = sessions[0].ID, sessions[1].ID
	records := []model.RuntimeInstance{batch, retained, missing}
	if err := executor.DB.Create(&records).Error; err != nil {
		t.Fatal(err)
	}
	for _, record := range []model.RuntimeInstance{batch, retained} {
		control.items[record.SandboxID] = remoteSandbox(record, record.State)
	}
	if err := executor.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"closed", "paused", "lost"} {
		if err := executor.DB.First(&records[i], records[i].ID).Error; err != nil || records[i].State != want {
			t.Fatalf("record %d: state=%s want=%s err=%v", i, records[i].State, want, err)
		}
	}
	if err := executor.DB.First(&sessions[1], sessions[1].ID).Error; err != nil || !sessions[1].WorkspaceLost {
		t.Fatalf("missing paused workspace was not marked unrecoverable: %+v %v", sessions[1], err)
	}
	control.mu.Lock()
	control.deleteNotFound = true
	control.mu.Unlock()
	for _, session := range sessions {
		task := engine.Task{Run: model.Run{ID: session.RunID}, Runner: runner}
		if err := executor.CloseSession(t.Context(), task, session); err != nil {
			t.Fatal(err)
		}
		if err := executor.CloseSession(t.Context(), task, session); err != nil {
			t.Fatalf("session cleanup is not idempotent: %v", err)
		}
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if _, ok := control.items[retained.SandboxID]; ok {
		t.Fatal("closed session retained its sandbox")
	}
}

func TestE2BRecoveryRejectsWrongDeploymentIdentity(t *testing.T) {
	executor, runner, control := e2bFixture(t)
	foreign := e2bRecord(t, executor, runner, 20, "running", "foreign-sandbox")
	foreign.DeploymentID = "another-deployment"
	if err := executor.DB.Create(&foreign).Error; err != nil {
		t.Fatal(err)
	}
	control.items[foreign.SandboxID] = remoteSandbox(foreign, "running")
	if err := executor.Recover(t.Context()); err == nil || !strings.Contains(err.Error(), "deployment") {
		t.Fatalf("scheduler recovery accepted a mismatched deployment identity: %v", err)
	}
	if err := executor.DB.First(&foreign, foreign.ID).Error; err != nil || foreign.State != "running" {
		t.Fatalf("another deployment's execution was modified: %+v %v", foreign, err)
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if len(control.requests) != 0 {
		t.Fatalf("recovery accessed another deployment's sandbox: %v", control.requests)
	}
}

func TestE2BRefusesCleanupWithMismatchedOwnership(t *testing.T) {
	executor, runner, control := e2bFixture(t)
	record := e2bRecord(t, executor, runner, 20, "running", "reassigned-sandbox")
	if err := executor.DB.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	remote := remoteSandbox(record, "running")
	remote.Metadata["cloudpilot_attempt"] = "another-attempt"
	control.items[remote.ID] = remote
	if err := executor.Recover(t.Context()); err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("recovery accepted inconsistent ownership: %v", err)
	}
	if err := executor.DB.First(&record, record.ID).Error; err != nil || record.State != "running" || !strings.Contains(record.LastError, "ownership") {
		t.Fatalf("failed cleanup was not retained: %+v %v", record, err)
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	if slices.Contains(control.requests, "DELETE /sandboxes/"+remote.ID) {
		t.Fatal("cleanup deleted an environment owned by another execution")
	}
}

func TestE2BRunErrorPreservesCancellationCause(t *testing.T) {
	workerErr := errors.New("worker failed")
	if got := sandboxRunError(t.Context(), t.Context(), workerErr); !errors.Is(got, workerErr) {
		t.Fatalf("normal worker failure was replaced: %v", got)
	}
	if got := sandboxRunError(t.Context(), t.Context(), nil); got != nil {
		t.Fatalf("successful execution returned an error: %v", got)
	}
	parent, cancelParent := context.WithCancel(t.Context())
	defer cancelParent()
	runCtx, cancelRun := context.WithCancelCause(parent)
	leaseErr := errors.New("sandbox lease renewal failed")
	cancelRun(leaseErr)
	if got := sandboxRunError(parent, runCtx, context.Canceled); !errors.Is(got, leaseErr) || errors.Is(got, context.Canceled) {
		t.Fatalf("lease failure was incorrectly reported as operator cancellation: %v", got)
	}
	cancelParent()
	if got := sandboxRunError(parent, runCtx, leaseErr); !errors.Is(got, context.Canceled) {
		t.Fatalf("operator cancellation lost its identity: %v", got)
	}
}

func TestE2BExecutionWaitsForMaintenanceAndCanCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := &E2B{}
		releaseMaintenance, err := executor.claim(1)
		if err != nil {
			t.Fatal(err)
		}
		releaseMaintenance = sync.OnceFunc(releaseMaintenance)
		defer releaseMaintenance()
		type result struct {
			release func()
			err     error
		}
		waiting := make(chan result, 1)
		go func() {
			release, err := executor.waitClaim(t.Context(), 1)
			waiting <- result{release, err}
		}()
		synctest.Wait()
		select {
		case got := <-waiting:
			if got.release != nil {
				got.release()
			}
			t.Fatalf("execution did not wait for maintenance: %v", got.err)
		default:
		}
		otherRelease, err := executor.claim(2)
		if err != nil {
			t.Fatalf("maintenance blocked an unrelated run: %v", err)
		}
		otherRelease()
		releaseMaintenance()
		synctest.Wait()
		got := <-waiting
		if got.err != nil || got.release == nil {
			t.Fatalf("execution did not acquire the released run: %v", got.err)
		}
		defer got.release()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		cancelled := make(chan result, 1)
		go func() {
			release, err := executor.waitClaim(ctx, 1)
			cancelled <- result{release, err}
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		stopped := <-cancelled
		if !errors.Is(stopped.err, context.Canceled) || stopped.release != nil {
			t.Fatalf("waiting execution ignored cancellation: %v", stopped.err)
		}
		if release, err := executor.claim(1); err == nil {
			release()
			t.Fatal("cancelling a waiter released the active execution")
		}
	})
}
