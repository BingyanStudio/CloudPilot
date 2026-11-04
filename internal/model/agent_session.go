package model

import (
	"encoding/json"
	"time"
)

// ExecutionSnapshot keeps a session's resource configuration stable across human follow-ups.
type ExecutionSnapshot struct {
	Pipeline  PipelineSpec  `json:"pipeline"`
	Worker    WorkerSpec    `json:"worker"`
	Workspace WorkspaceSpec `json:"workspace"`
	Runner    RunnerSpec    `json:"runner"`
}

// AgentSession tracks the provider conversation and the retained execution workspace.
type AgentSession struct {
	ID             uint            `gorm:"primaryKey" json:"id"`
	RunID          uint            `gorm:"uniqueIndex" json:"runId"`
	ThreadID       string          `gorm:"size:190" json:"threadId"`
	State          string          `gorm:"size:32;index" json:"state"`
	CurrentTurnID  uint            `json:"currentTurnId"`
	Snapshot       json.RawMessage `gorm:"serializer:json" json:"-"`
	WorkspaceLost  bool            `json:"workspaceLost"`
	LastError      string          `json:"lastError"`
	InputTokens    int64           `json:"inputTokens"`
	OutputTokens   int64           `json:"outputTokens"`
	TotalTokens    int64           `json:"totalTokens"`
	UsageUpdatedAt *time.Time      `json:"usageUpdatedAt"`
	LastActivityAt *time.Time      `json:"lastActivityAt"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	ClosedAt       *time.Time      `json:"closedAt"`
}

// AgentTurn is one durable unit of work within an interactive run.
type AgentTurn struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	RunID          uint       `gorm:"index" json:"runId"`
	SessionID      uint       `gorm:"index;uniqueIndex:idx_session_turn" json:"sessionId"`
	Number         int        `gorm:"uniqueIndex:idx_session_turn" json:"number"`
	ProviderTurnID string     `gorm:"size:190" json:"providerTurnId"`
	Input          string     `json:"input"`
	Initial        bool       `json:"initial"`
	UserID         uint       `json:"userId"`
	Status         string     `gorm:"size:32;index" json:"status"`
	CreatedAt      time.Time  `json:"createdAt"`
	StartedAt      *time.Time `json:"startedAt"`
	FinishedAt     *time.Time `json:"finishedAt"`
}

// SessionEvent is an append-only message or tool update; ItemID joins deltas within a turn.
type SessionEvent struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	RunID     uint      `gorm:"index" json:"runId"`
	TurnID    uint      `gorm:"index" json:"turnId"`
	Type      string    `gorm:"size:32" json:"type"`
	ItemID    string    `gorm:"size:190" json:"itemId"`
	Role      string    `gorm:"size:32" json:"role"`
	Status    string    `gorm:"size:32" json:"status"`
	Text      string    `json:"text"`
	Tool      string    `gorm:"size:190" json:"tool"`
	Command   string    `json:"command"`
	Output    string    `json:"output"`
	Delta     bool      `json:"delta"`
	ControlID uint      `json:"controlId"`
	CreatedAt time.Time `json:"createdAt"`
}
