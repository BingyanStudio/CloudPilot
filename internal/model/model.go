package model

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
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

// Document returns a public resource without stored API credentials.
func (r Resource) Document() Document {
	doc := Document{APIVersion: APIVersion, Kind: r.Kind, Metadata: Metadata{Name: r.Name, Generation: r.Generation}, Spec: r.Spec}
	if r.Kind == "Worker" || r.Kind == "Runner" || r.Kind == "Input" {
		var spec map[string]any
		if err := json.Unmarshal(r.Spec, &spec); err != nil {
			doc.Spec = json.RawMessage(`{}`)
			return doc
		}
		delete(spec, "apiKey")
		delete(spec, "secret")
		if github, ok := spec["github"].(map[string]any); ok {
			delete(github, "token")
		}
		if e2b, ok := spec["e2b"].(map[string]any); ok {
			delete(e2b, "apiKey")
		}
		doc.Spec, _ = json.Marshal(spec)
	}
	return doc
}

func (r Resource) Decode(target any) error { return json.Unmarshal(r.Spec, target) }

func FindResource(db *gorm.DB, kind, name string) (Resource, error) {
	var r Resource
	err := db.Where("kind = ? AND name = ?", kind, name).First(&r).Error
	return r, err
}

type InputSpec struct {
	Type        string           `json:"type"`
	PipelineRef string           `json:"pipelineRef"`
	Secret      string           `json:"secret,omitempty"`
	SecretRef   string           `json:"secretRef,omitempty"`
	Paused      bool             `json:"paused"`
	GitHub      *GitHubInputSpec `json:"github,omitempty"`
}

type GitHubInputSpec struct {
	Repository     string   `json:"repository"`
	Token          string   `json:"token,omitempty"`
	TokenSecretRef string   `json:"tokenSecretRef,omitempty"`
	Assignees      []string `json:"assignees,omitempty"`
	Labels         []string `json:"labels,omitempty"`
}

type PipelineSpec struct {
	WorkerRef           string `json:"workerRef"`
	WorkspaceRef        string `json:"workspaceRef"`
	RunnerRef           string `json:"runnerRef"`
	Schedule            string `json:"schedule"`
	Timezone            string `json:"timezone"`
	Concurrency         int    `json:"concurrency"`
	Paused              bool   `json:"paused"`
	MaxRetries          int    `json:"maxRetries"`
	TimeoutSeconds      int    `json:"timeoutSeconds"`
	StallTimeoutSeconds int    `json:"stallTimeoutSeconds"`
}

type WorkerSpec struct {
	Type            string `json:"type"`
	Model           string `json:"model"`
	Endpoint        string `json:"endpoint"`
	APIKey          string `json:"apiKey,omitempty"`
	APIKeySecretRef string `json:"apiKeySecretRef,omitempty"`
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
	APIKey          string `json:"apiKey,omitempty"`
	APIKeySecretRef string `json:"apiKeySecretRef,omitempty"`
	Template        string `json:"template"`
	LeaseSeconds    int    `json:"leaseSeconds"`
	IdleStrategy    string `json:"idleStrategy"`
}

const (
	RoleViewer = "viewer"
	RoleEditor = "editor"
	RoleAdmin  = "admin"
)

// RoleRank orders roles so that a higher rank includes every lower permission; unknown roles rank zero.
func RoleRank(role string) int {
	switch role {
	case RoleViewer:
		return 1
	case RoleEditor:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

type User struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	Username     string    `gorm:"size:190;uniqueIndex" json:"username"`
	PasswordHash string    `gorm:"size:255" json:"-"`
	Role         string    `gorm:"size:16;not null;default:viewer" json:"role"`
	OIDCSubject  *string   `gorm:"column:oidc_subject;size:64;uniqueIndex" json:"-"`
	SSO          bool      `gorm:"-" json:"sso"`
	CreatedAt    time.Time `json:"createdAt"`
}

func (u *User) AfterFind(*gorm.DB) error {
	u.SSO = u.OIDCSubject != nil
	return nil
}

// OIDCSettings is the single-row SSO configuration managed from the web console.
type OIDCSettings struct {
	ID           uint   `gorm:"primaryKey" json:"-"`
	Enabled      bool   `json:"enabled"`
	DiscoveryURL string `json:"discoveryUrl"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"-"`
	Scopes       string `json:"scopes"`
	DefaultRole  string `gorm:"size:16" json:"defaultRole"`
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
	OutputPath  string    `gorm:"-" json:"outputPath,omitempty"`
	Content     string    `json:"content"`
	ContentType string    `gorm:"size:255" json:"contentType"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"createdAt"`
}

// AfterFind exposes a file's path inside the worker's /output directory so agent links can be resolved.
func (a *Artifact) AfterFind(*gorm.DB) error {
	parts := strings.Split(filepath.ToSlash(a.Path), "/")
	for i := 1; i < len(parts)-1; i++ {
		if _, err := strconv.ParseUint(parts[i-1], 10, 64); err == nil && strings.HasPrefix(parts[i], "artifacts-") {
			a.OutputPath = strings.Join(parts[i+1:], "/")
			break
		}
	}
	return nil
}

type PipelineState struct {
	ID              uint   `gorm:"primaryKey"`
	Name            string `gorm:"size:190;uniqueIndex"`
	LastScheduledAt *time.Time
	DrainUntil      *time.Time
}
