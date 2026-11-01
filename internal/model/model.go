package model

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

const APIVersion = "cloudpilot.dev/v1alpha1"

type Metadata struct {
	Name       string `json:"name" yaml:"name"`
	Generation int64  `json:"generation,omitempty" yaml:"generation,omitempty"`
}

type Document struct {
	APIVersion string          `json:"apiVersion" yaml:"apiVersion"`
	Kind       string          `json:"kind" yaml:"kind"`
	Metadata   Metadata        `json:"metadata" yaml:"metadata"`
	Spec       json.RawMessage `json:"spec" yaml:"spec"`
}

type Resource struct {
	ID         uint            `gorm:"primaryKey" json:"id"`
	Kind       string          `gorm:"size:32;uniqueIndex:idx_resource_name" json:"kind"`
	Name       string          `gorm:"size:190;uniqueIndex:idx_resource_name" json:"name"`
	Spec       json.RawMessage `gorm:"serializer:json" json:"spec"`
	Generation int64           `json:"generation"`
	CreatedAt  time.Time       `json:"createdAt"`
	UpdatedAt  time.Time       `json:"updatedAt"`
}

func (r Resource) Document() Document {
	return Document{APIVersion: APIVersion, Kind: r.Kind, Metadata: Metadata{Name: r.Name, Generation: r.Generation}, Spec: r.Spec}
}

func (r Resource) Decode(target any) error { return json.Unmarshal(r.Spec, target) }

func FindResource(db *gorm.DB, kind, name string) (Resource, error) {
	var r Resource
	err := db.Where("kind = ? AND name = ?", kind, name).First(&r).Error
	return r, err
}

type InputSpec struct {
	Type        string `json:"type"`
	PipelineRef string `json:"pipelineRef"`
	SecretRef   string `json:"secretRef"`
	Paused      bool   `json:"paused"`
}

type PipelineSpec struct {
	WorkerRef      string `json:"workerRef"`
	WorkspaceRef   string `json:"workspaceRef"`
	RunnerRef      string `json:"runnerRef"`
	Schedule       string `json:"schedule"`
	Timezone       string `json:"timezone"`
	Concurrency    int    `json:"concurrency"`
	Paused         bool   `json:"paused"`
	MaxRetries     int    `json:"maxRetries"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

type WorkerSpec struct {
	Type            string `json:"type"`
	Model           string `json:"model"`
	Endpoint        string `json:"endpoint"`
	APIKeySecretRef string `json:"apiKeySecretRef"`
	Instructions    string `json:"instructions"`
}

type WorkspaceSpec struct {
	Repositories []Repository         `json:"repositories"`
	Prompt       string               `json:"prompt"`
	Tools        []string             `json:"tools"`
	MCPServers   map[string]MCPServer `json:"mcpServers"`
	Env          map[string]string    `json:"env"`
	SecretEnv    map[string]string    `json:"secretEnv"`
}

type Repository struct {
	URL  string `json:"url"`
	Ref  string `json:"ref"`
	Path string `json:"path"`
}

type MCPServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
}

type RunnerSpec struct {
	Type    string         `json:"type"`
	Image   string         `json:"image,omitempty"`
	Network string         `json:"network,omitempty"`
	Memory  string         `json:"memory,omitempty"`
	CPUs    string         `json:"cpus,omitempty"`
	E2B     *E2BRunnerSpec `json:"e2b,omitempty"`
}

type E2BRunnerSpec struct {
	Provider        string `json:"provider"`
	Domain          string `json:"domain"`
	APIURL          string `json:"apiUrl,omitempty"`
	APIKeySecretRef string `json:"apiKeySecretRef"`
	Template        string `json:"template"`
	LeaseSeconds    int    `json:"leaseSeconds"`
	IdleStrategy    string `json:"idleStrategy"`
}

type User struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	Username     string    `gorm:"size:190;uniqueIndex" json:"username"`
	PasswordHash string    `gorm:"size:255" json:"-"`
	CreatedAt    time.Time `json:"createdAt"`
}

type Session struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	TokenHash string    `gorm:"size:64;uniqueIndex" json:"-"`
	UserID    uint      `gorm:"index" json:"userId"`
	ExpiresAt time.Time `gorm:"index" json:"expiresAt"`
}

type Event struct {
	ID             uint            `gorm:"primaryKey" json:"id"`
	InputName      string          `gorm:"size:190;uniqueIndex:idx_event_idempotency" json:"inputName"`
	PipelineName   string          `gorm:"size:190;index" json:"pipelineName"`
	Status         string          `gorm:"size:32;index" json:"status"`
	Payload        json.RawMessage `gorm:"serializer:json" json:"payload"`
	IdempotencyKey *string         `gorm:"size:190;uniqueIndex:idx_event_idempotency" json:"-"`
	Attempts       int             `json:"attempts"`
	AvailableAt    time.Time       `gorm:"index" json:"availableAt"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
}

type Run struct {
	ID            uint       `gorm:"primaryKey" json:"id"`
	EventID       uint       `gorm:"index" json:"eventId"`
	PipelineName  string     `gorm:"size:190;index" json:"pipelineName"`
	Status        string     `gorm:"size:32;index" json:"status"`
	Attempt       int        `json:"attempt"`
	WorkerName    string     `gorm:"size:190" json:"workerName"`
	WorkspaceName string     `gorm:"size:190" json:"workspaceName"`
	RunnerName    string     `gorm:"size:190" json:"runnerName"`
	Error         string     `json:"error"`
	StartedAt     time.Time  `json:"startedAt"`
	FinishedAt    *time.Time `json:"finishedAt"`
	CreatedAt     time.Time  `json:"createdAt"`
}

type RunLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	RunID     uint      `gorm:"index" json:"runId"`
	Stream    string    `gorm:"size:32" json:"stream"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"createdAt"`
}

type Artifact struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	RunID       uint      `gorm:"index" json:"runId"`
	Kind        string    `gorm:"size:32" json:"kind"`
	Name        string    `gorm:"size:255" json:"name"`
	URL         string    `json:"url"`
	Path        string    `json:"-"`
	Content     string    `json:"content"`
	ContentType string    `gorm:"size:255" json:"contentType"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"createdAt"`
}

type PipelineState struct {
	ID              uint   `gorm:"primaryKey"`
	Name            string `gorm:"size:190;uniqueIndex"`
	LastScheduledAt *time.Time
	DrainUntil      *time.Time
}
