package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/engine"
	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/BingyanStudio/CloudPilot/internal/sandbox"
)

func (e *E2B) deployment() (string, error) {
	return deploymentID(filepath.Join(e.DataDir, "e2b"))
}

func (e *E2B) provision(ctx context.Context, client *sandbox.Client, task engine.Task, sessionID uint) (*model.RuntimeInstance, *sandbox.Sandbox, error) {
	deployment, err := e.deployment()
	if err != nil {
		return nil, nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, nil, err
	}
	snapshot, err := json.Marshal(task.Runner)
	if err != nil {
		return nil, nil, err
	}
	lease := sandboxLease(task.Runner)
	expires := time.Now().UTC().Add(lease + time.Minute)
	record := &model.RuntimeInstance{DeploymentID: deployment, EventID: task.Event.ID, RunID: task.Run.ID, SessionID: sessionID, AttemptID: hex.EncodeToString(nonce[:]), RunnerSnapshot: snapshot, State: "creating", ExpiresAt: &expires}
	if err := e.DB.Create(record).Error; err != nil {
		return nil, nil, err
	}
	metadata := map[string]string{"cloudpilot_instance": deployment, "cloudpilot_attempt": record.AttemptID, "cloudpilot_run": strconv.FormatUint(uint64(record.RunID), 10), "cloudpilot_event": strconv.FormatUint(uint64(record.EventID), 10), "cloudpilot_session": strconv.FormatUint(uint64(sessionID), 10)}
	// Wait for the create response after local cancellation so its remote resource can be reclaimed.
	createCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	sb, err := client.Create(createCtx, sandbox.CreateOptions{TemplateID: task.Runner.E2B.Template, Timeout: lease, Metadata: metadata, PauseOnTimeout: sessionID != 0})
	cancel()
	if err != nil {
		var response *sandbox.HTTPError
		if errors.As(err, &response) && response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusRequestTimeout {
			_ = e.closed(record)
		}
		return record, nil, fmt.Errorf("create sandbox: %w", err)
	}
	record.SandboxID, record.State = sb.ID, "running"
	expires = time.Now().UTC().Add(lease)
	record.ExpiresAt = &expires
	if err := e.DB.Model(record).Updates(map[string]any{"sandbox_id": sb.ID, "state": record.State, "expires_at": expires}).Error; err != nil {
		return record, sb, fmt.Errorf("persist sandbox: %w", err)
	}
	return record, sb, ctx.Err()
}

func (e *E2B) sessionSandbox(ctx context.Context, client *sandbox.Client, task engine.Task, session model.AgentSession) (*model.RuntimeInstance, *sandbox.Sandbox, error) {
	if session.ID == 0 || session.RunID != task.Run.ID {
		return nil, nil, errors.New("sandbox sessions require a persisted session identity")
	}
	deployment, err := e.deployment()
	if err != nil {
		return nil, nil, err
	}
	var records []model.RuntimeInstance
	if err := e.DB.Where("deployment_id = ? AND session_id = ? AND state <> ?", deployment, session.ID, "closed").Order("id DESC").Limit(1).Find(&records).Error; err != nil {
		return nil, nil, err
	}
	if len(records) == 0 {
		if session.ThreadID != "" {
			return nil, nil, e.workspaceLost(session.ID, nil)
		}
		return e.provision(ctx, client, task, session.ID)
	}
	record := &records[0]
	if record.State == "lost" {
		return record, nil, e.workspaceLost(session.ID, record)
	}
	if err := e.resolvePending(ctx, client, record); err != nil {
		return record, nil, err
	}
	if record.State == "closed" {
		if session.ThreadID != "" {
			return nil, nil, e.workspaceLost(session.ID, nil)
		}
		return e.provision(ctx, client, task, session.ID)
	}
	// Reconcile a failed previous teardown before starting another turn.
	if err := e.settle(ctx, client, record, true); err != nil {
		return record, nil, err
	}
	sb, err := client.Connect(ctx, record.SandboxID, sandboxLease(task.Runner))
	if sandbox.IsNotFound(err) {
		return record, nil, e.workspaceLost(session.ID, record)
	}
	if err != nil {
		return record, nil, fmt.Errorf("resume sandbox: %w", err)
	}
	if sb.ID != record.SandboxID {
		return record, nil, errors.New("provider connected to an unexpected sandbox")
	}
	record.State = "running"
	record.ProcessID = 0
	expires := time.Now().UTC().Add(sandboxLease(task.Runner))
	if err := e.DB.Model(record).Updates(map[string]any{"state": "running", "process_id": 0, "last_error": "", "expires_at": expires}).Error; err != nil {
		return record, sb, err
	}
	return record, sb, nil
}

func ownsSandbox(sb *sandbox.Sandbox, record *model.RuntimeInstance) error {
	if sb.Metadata["cloudpilot_instance"] != record.DeploymentID || sb.Metadata["cloudpilot_attempt"] != record.AttemptID {
		return errors.New("sandbox ownership metadata does not match this execution")
	}
	return nil
}

func (e *E2B) clearPrevious(ctx context.Context, task engine.Task) error {
	deployment, err := e.deployment()
	if err != nil {
		return err
	}
	var records []model.RuntimeInstance
	query := e.DB.Where("deployment_id = ? AND state NOT IN ?", deployment, []string{"closed", "lost"})
	if task.Event.ID != 0 {
		query = query.Where("event_id = ?", task.Event.ID)
	} else {
		query = query.Where("run_id = ?", task.Run.ID)
	}
	if err := query.Find(&records).Error; err != nil {
		return err
	}
	for i := range records {
		if records[i].SessionID != 0 {
			return errors.New("close the retained sandbox session before retrying the event")
		}
		client, err := e.recordClient(&records[i])
		if err != nil {
			return err
		}
		if err := e.settle(ctx, client, &records[i], false); err != nil {
			return fmt.Errorf("stop previous sandbox before retry: %w", err)
		}
	}
	return nil
}

func (e *E2B) recordClient(record *model.RuntimeInstance) (*sandbox.Client, error) {
	var spec model.RunnerSpec
	if err := json.Unmarshal(record.RunnerSnapshot, &spec); err != nil {
		return nil, fmt.Errorf("read sandbox connection snapshot: %w", err)
	}
	return e.client(spec)
}

func (e *E2B) resolvePending(ctx context.Context, client *sandbox.Client, record *model.RuntimeInstance) error {
	if record.SandboxID != "" || record.State == "closed" || record.State == "lost" {
		return nil
	}
	matches, err := client.List(ctx, map[string]string{"cloudpilot_instance": record.DeploymentID, "cloudpilot_attempt": record.AttemptID})
	if err != nil {
		return fmt.Errorf("find sandbox after an uncertain create: %w", err)
	}
	for i := range matches {
		if err := ownsSandbox(&matches[i], record); err != nil {
			return err
		}
	}
	if len(matches) == 0 {
		if record.SessionID == 0 && record.ExpiresAt != nil && time.Now().After(*record.ExpiresAt) {
			return e.closed(record)
		}
		return errors.New("sandbox creation outcome is uncertain; cleanup must finish before retrying")
	}
	for _, extra := range matches[1:] {
		if err := client.Kill(ctx, extra.ID); err != nil {
			return err
		}
	}
	record.SandboxID = matches[0].ID
	return e.DB.Model(record).Update("sandbox_id", record.SandboxID).Error
}

func (e *E2B) cleanupRecord(record *model.RuntimeInstance, retain bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	return e.cleanupWithContext(ctx, record, retain)
}

func (e *E2B) cleanupWithContext(ctx context.Context, record *model.RuntimeInstance, retain bool) error {
	client, err := e.recordClient(record)
	if err == nil {
		err = e.settle(ctx, client, record, retain)
	}
	if err != nil {
		message := err.Error()
		if client != nil {
			message = redactor([]string{client.APIKey})(message)
		}
		_ = e.DB.Model(record).Update("last_error", message).Error
		return fmt.Errorf("sandbox cleanup pending: %s", message)
	}
	return nil
}

func (e *E2B) settle(ctx context.Context, client *sandbox.Client, record *model.RuntimeInstance, retain bool) error {
	if record.State == "closed" {
		return nil
	}
	if record.State == "lost" {
		if !retain {
			return e.closed(record)
		}
		return e.workspaceLost(record.SessionID, record)
	}
	if err := e.resolvePending(ctx, client, record); err != nil {
		return err
	}
	if record.State == "closed" {
		return nil
	}
	sb, err := client.Get(ctx, record.SandboxID)
	if sandbox.IsNotFound(err) {
		if retain {
			return e.workspaceLost(record.SessionID, record)
		}
		return e.closed(record)
	}
	if err != nil {
		return err
	}
	if err := ownsSandbox(sb, record); err != nil {
		return err
	}
	if !retain {
		record.State = "closing"
		if err := e.DB.Model(record).Update("state", "closing").Error; err != nil {
			return err
		}
		if err := client.Kill(ctx, record.SandboxID); err != nil {
			return err
		}
		if _, err := client.Get(ctx, record.SandboxID); !sandbox.IsNotFound(err) {
			if err != nil {
				return err
			}
			return errors.New("provider has not confirmed sandbox deletion")
		}
		return e.closed(record)
	}
	if record.State == "paused" && sb.State == "paused" {
		return nil
	}
	if sb.State == "paused" {
		sb, err = client.Connect(ctx, record.SandboxID, 5*time.Minute)
		if err != nil {
			return err
		}
	}
	if err := sandboxCommand(ctx, sb, "sh", []string{"-c", "if [ -f /tmp/cloudpilot-process.mjs ]; then exec node /tmp/cloudpilot-process.mjs stop; fi"}, nil); err != nil {
		return fmt.Errorf("stop sandbox worker group: %w", err)
	}
	if record.ProcessID != 0 {
		if err := sb.StopProcess(ctx, record.ProcessID); err != nil {
			return err
		}
	}
	if err := client.Pause(ctx, record.SandboxID); err != nil {
		return err
	}
	current, err := client.Get(ctx, record.SandboxID)
	if err != nil {
		return err
	}
	if current.State != "paused" {
		return errors.New("provider has not confirmed sandbox pause")
	}
	record.State, record.ProcessID = "paused", 0
	return e.DB.Model(record).Updates(map[string]any{"state": "paused", "process_id": 0, "expires_at": nil, "last_error": ""}).Error
}

func (e *E2B) closed(record *model.RuntimeInstance) error {
	record.State, record.ProcessID = "closed", 0
	return e.DB.Model(record).Updates(map[string]any{"state": "closed", "process_id": 0, "expires_at": nil, "last_error": "", "closed_at": time.Now().UTC()}).Error
}

func (e *E2B) workspaceLost(sessionID uint, record *model.RuntimeInstance) error {
	message := "sandbox workspace is unavailable; close this session and start a new run"
	var persistErr error
	if record != nil {
		record.State = "lost"
		persistErr = e.DB.Model(record).Updates(map[string]any{"state": "lost", "last_error": message}).Error
	}
	if sessionID != 0 {
		persistErr = errors.Join(persistErr, e.DB.Model(&model.AgentSession{}).Where("id = ?", sessionID).Updates(map[string]any{"workspace_lost": true, "last_error": message}).Error)
	}
	return errors.Join(errors.New(message), persistErr)
}

// CloseSession deletes the session sandbox and treats an already missing workspace as closed.
func (e *E2B) CloseSession(ctx context.Context, task engine.Task, session model.AgentSession) error {
	if session.ID == 0 || task.Run.ID == 0 || session.RunID != task.Run.ID {
		return errors.New("invalid sandbox session identity")
	}
	release, err := e.waitClaim(ctx, task.Run.ID)
	if err != nil {
		return err
	}
	defer release()
	deployment, err := e.deployment()
	if err != nil {
		return err
	}
	var records []model.RuntimeInstance
	if err := e.DB.Where("deployment_id = ? AND session_id = ? AND run_id = ? AND state <> ?", deployment, session.ID, task.Run.ID, "closed").Find(&records).Error; err != nil {
		return err
	}
	for i := range records {
		client, err := e.recordClient(&records[i])
		if err != nil {
			return err
		}
		if err := e.settle(ctx, client, &records[i], false); err != nil {
			return redactError(err, redactor([]string{client.APIKey}))
		}
	}
	return nil
}

// Recover stops interrupted executions before the scheduler can retry their events.
func (e *E2B) Recover(ctx context.Context) error {
	if e.DB == nil {
		return errors.New("E2B runner database is required")
	}
	var count int64
	if err := e.DB.Model(&model.RuntimeInstance{}).Where("state NOT IN ?", []string{"closed", "lost"}).Count(&count).Error; err != nil || count == 0 {
		return err
	}
	if _, err := os.Stat(filepath.Join(e.DataDir, "e2b", "runtime-id")); err != nil {
		return fmt.Errorf("restore the E2B deployment runtime-id before recovering sandboxes: %w", err)
	}
	deployment, err := e.deployment()
	if err != nil {
		return err
	}
	if err := e.DB.Model(&model.RuntimeInstance{}).Where("state NOT IN ? AND deployment_id <> ?", []string{"closed", "lost"}, deployment).Count(&count).Error; err != nil {
		return err
	}
	if count != 0 {
		return errors.New("sandbox records belong to another deployment; restore the matching E2B runtime-id with the database")
	}
	return e.reconcile(ctx)
}

func (e *E2B) reconcile(ctx context.Context) error {
	deployment, err := e.deployment()
	if err != nil {
		return err
	}
	var records []model.RuntimeInstance
	if err := e.DB.Where("deployment_id = ? AND state NOT IN ?", deployment, []string{"closed", "lost"}).Find(&records).Error; err != nil {
		return err
	}
	var failures []error
	for i := range records {
		record := &records[i]
		release, err := e.claim(record.RunID)
		if err != nil {
			continue
		}
		retain := record.SessionID != 0
		if retain {
			var session model.AgentSession
			if err := e.DB.First(&session, record.SessionID).Error; err != nil {
				release()
				failures = append(failures, err)
				continue
			}
			retain = session.State != "closed" && session.State != "closing"
		}
		cleanupCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		err = e.cleanupWithContext(cleanupCtx, record, retain)
		cancel()
		release()
		if err != nil && record.State != "lost" {
			failures = append(failures, err)
		}
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
	}
	return errors.Join(failures...)
}

// Maintain retries incomplete cleanup and discovers workspaces removed outside CloudPilot.
func (e *E2B) Maintain(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.reconcile(ctx); err != nil {
				slog.Error("sandbox reconciliation failed", "error", err)
			}
		}
	}
}
