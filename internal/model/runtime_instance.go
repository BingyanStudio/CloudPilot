package model

import (
	"encoding/json"
	"time"
)

// RuntimeInstance records a remote execution environment from creation through cleanup.
// RunnerSnapshot contains connection settings and credential references, never resolved credentials.
type RuntimeInstance struct {
	ID             uint            `gorm:"primaryKey" json:"id"`
	DeploymentID   string          `gorm:"size:64;index:idx_runtime_deployment_state" json:"deploymentId"`
	EventID        uint            `gorm:"index" json:"eventId"`
	RunID          uint            `gorm:"index" json:"runId"`
	SessionID      uint            `gorm:"index" json:"sessionId"`
	AttemptID      string          `gorm:"size:64;uniqueIndex" json:"attemptId"`
	RunnerSnapshot json.RawMessage `gorm:"serializer:json" json:"-"`
	SandboxID      string          `gorm:"size:190;index" json:"sandboxId"`
	ProcessID      uint32          `json:"processId"`
	State          string          `gorm:"size:32;index:idx_runtime_deployment_state" json:"state"`
	ExpiresAt      *time.Time      `json:"expiresAt"`
	LastRenewedAt  *time.Time      `json:"lastRenewedAt"`
	LastError      string          `json:"lastError"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	ClosedAt       *time.Time      `json:"closedAt"`
}
