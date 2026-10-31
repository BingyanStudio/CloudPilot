package resource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"
	"gorm.io/gorm"
)

const apiVersion = "cloudpilot.dev/v1alpha1"

var (
	mutation      sync.Mutex
	namePattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	envPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	ErrReferenced = errors.New("resource is still referenced")
	ErrConflict   = errors.New("resource changed; reload before saving")
)

// ParseYAML accepts strict, Kubernetes-style multi-document resource manifests.
func ParseYAML(data []byte) ([]model.Document, error) {
	if len(data) > 1<<20 {
		return nil, errors.New("configuration exceeds 1 MiB")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	docs := make([]model.Document, 0)
	for {
		var value any
		if err := decoder.Decode(&value); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("invalid YAML: %w", err)
		}
		if value == nil {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("YAML keys must be strings: %w", err)
		}
		var doc model.Document
		if err := decodeStrict(encoded, &doc); err != nil {
			return nil, fmt.Errorf("invalid document: %w", err)
		}
		docs = append(docs, doc)
		if len(docs) > 100 {
			return nil, errors.New("at most 100 resources per import")
		}
	}
	if len(docs) == 0 {
		return nil, errors.New("configuration contains no resources")
	}
	return docs, nil
}

// ExportYAML produces portable manifests without persistence metadata or secret values.
func ExportYAML(docs []model.Document) ([]byte, error) {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	for _, doc := range docs {
		doc.Metadata.Generation = 0
		data, err := json.Marshal(doc)
		if err != nil {
			return nil, err
		}
		var value map[string]any
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, err
		}
		if err := encoder.Encode(value); err != nil {
			return nil, err
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Apply validates the entire resource graph before atomically storing a batch.
func Apply(db *gorm.DB, docs []model.Document) ([]model.Document, error) {
	if len(docs) == 0 || len(docs) > 100 {
		return nil, errors.New("apply requires between 1 and 100 resources")
	}
	mutation.Lock()
	defer mutation.Unlock()
	result := make([]model.Document, 0, len(docs))
	err := db.Transaction(func(tx *gorm.DB) error {
		var stored []model.Resource
		if err := tx.Find(&stored).Error; err != nil {
			return err
		}
		graph := make(map[string]model.Document, len(stored)+len(docs))
		for _, item := range stored {
			graph[key(item.Kind, item.Name)] = item.Document()
		}
		batch := make(map[string]bool, len(docs))
		for _, original := range docs {
			doc, err := normalize(original)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", original.Kind, original.Metadata.Name, err)
			}
			id := key(doc.Kind, doc.Metadata.Name)
			if batch[id] {
				return fmt.Errorf("duplicate resource %s", id)
			}
			batch[id] = true
			if old, exists := graph[id]; exists && doc.Metadata.Generation != 0 && doc.Metadata.Generation != old.Metadata.Generation {
				return ErrConflict
			}
			graph[id] = doc
			result = append(result, doc)
		}
		for _, doc := range graph {
			for _, ref := range references(doc) {
				if _, ok := graph[ref]; !ok {
					return fmt.Errorf("%s references missing %s", key(doc.Kind, doc.Metadata.Name), ref)
				}
			}
		}
		for i, doc := range result {
			var item model.Resource
			err := tx.Where("kind = ? AND name = ?", doc.Kind, doc.Metadata.Name).First(&item).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			item.Kind, item.Name, item.Spec = doc.Kind, doc.Metadata.Name, doc.Spec
			item.Generation++
			if err := tx.Save(&item).Error; err != nil {
				return err
			}
			result[i] = item.Document()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Delete rejects removal while another resource or queued execution depends on it.
func Delete(db *gorm.DB, kind, name string) error {
	mutation.Lock()
	defer mutation.Unlock()
	return db.Transaction(func(tx *gorm.DB) error {
		item, err := model.FindResource(tx, kind, name)
		if err != nil {
			return err
		}
		var resources []model.Resource
		if err := tx.Find(&resources).Error; err != nil {
			return err
		}
		for _, other := range resources {
			if slices.Contains(references(other.Document()), key(kind, name)) {
				return fmt.Errorf("%w by %s/%s", ErrReferenced, other.Kind, other.Name)
			}
		}
		if kind == "Pipeline" {
			var count int64
			if err := tx.Model(&model.Event{}).Where("pipeline_name = ? AND status IN ?", name, []string{"queued", "running"}).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				return fmt.Errorf("%w by queued or running events", ErrReferenced)
			}
			if err := tx.Model(&model.AgentSession{}).Joins("JOIN runs ON runs.id = agent_sessions.run_id").Where("runs.pipeline_name = ? AND agent_sessions.state <> ?", name, "closed").Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				return fmt.Errorf("%w by open agent sessions; close them before deleting the pipeline", ErrReferenced)
			}
		}
		return tx.Delete(&item).Error
	})
}

func key(kind, name string) string { return kind + "/" + name }

func decodeStrict(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON value")
	}
	return nil
}

func normalize(doc model.Document) (model.Document, error) {
	if doc.APIVersion != apiVersion {
		return doc, fmt.Errorf("apiVersion must be %s", apiVersion)
	}
	if !namePattern.MatchString(doc.Metadata.Name) {
		return doc, errors.New("name must contain 1–63 lowercase letters, digits or hyphens and start/end with a letter or digit")
	}
	if len(doc.Spec) == 0 || bytes.Equal(bytes.TrimSpace(doc.Spec), []byte("null")) {
		return doc, errors.New("spec is required")
	}
	var spec any
	switch doc.Kind {
	case "Input":
		var s model.InputSpec
		if err := decodeStrict(doc.Spec, &s); err != nil {
			return doc, err
		}
		if s.Type == "" {
			s.Type = "webhook"
		}
		if s.Type != "webhook" {
			return doc, errors.New("input type must be webhook")
		}
		if !namePattern.MatchString(s.PipelineRef) || !envPattern.MatchString(s.SecretRef) {
			return doc, errors.New("pipelineRef and secretRef (environment variable name) are required")
		}
		spec = s
	case "Pipeline":
		var s model.PipelineSpec
		if err := decodeStrict(doc.Spec, &s); err != nil {
			return doc, err
		}
		if !namePattern.MatchString(s.WorkerRef) || !namePattern.MatchString(s.WorkspaceRef) || !namePattern.MatchString(s.RunnerRef) {
			return doc, errors.New("workerRef, workspaceRef and runnerRef are required")
		}
		if s.Concurrency == 0 {
			s.Concurrency = 1
		}
		if s.Concurrency < 1 || s.Concurrency > 32 {
			return doc, errors.New("concurrency must be between 1 and 32")
		}
		if s.MaxRetries < 0 || s.MaxRetries > 10 {
			return doc, errors.New("maxRetries must be between 0 and 10")
		}
		if s.TimeoutSeconds == 0 {
			s.TimeoutSeconds = 3600
		}
		if s.TimeoutSeconds < 10 || s.TimeoutSeconds > 86400 {
			return doc, errors.New("timeoutSeconds must be between 10 and 86400")
		}
		if s.Timezone == "" {
			s.Timezone = "Asia/Shanghai"
		}
		if _, err := time.LoadLocation(s.Timezone); err != nil {
			return doc, errors.New("timezone must be a valid IANA timezone")
		}
		if s.Schedule != "" {
			if strings.HasPrefix(s.Schedule, "TZ=") || strings.HasPrefix(s.Schedule, "CRON_TZ=") {
				return doc, errors.New("set timezone separately from schedule")
			}
			if _, err := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse(s.Schedule); err != nil {
				return doc, fmt.Errorf("invalid five-field cron schedule: %w", err)
			}
		}
		spec = s
	case "Worker":
		var s model.WorkerSpec
		if err := decodeStrict(doc.Spec, &s); err != nil {
			return doc, err
		}
		if s.Type == "" {
			s.Type = "codex"
		}
		if s.Type != "codex" {
			return doc, errors.New("worker type must be codex")
		}
		if strings.TrimSpace(s.Model) == "" || strings.ContainsAny(s.Model, "\r\n") {
			return doc, errors.New("model is required and must be a single line")
		}
		if !envPattern.MatchString(s.APIKeySecretRef) {
			return doc, errors.New("apiKeySecretRef must name an environment variable")
		}
		if s.Endpoint != "" && !validHTTPURL(s.Endpoint) {
			return doc, errors.New("endpoint must be an absolute HTTP(S) URL without embedded credentials")
		}
		spec = s
	case "Workspace":
		var s model.WorkspaceSpec
		if err := decodeStrict(doc.Spec, &s); err != nil {
			return doc, err
		}
		paths := map[string]bool{}
		for i := range s.Repositories {
			r := &s.Repositories[i]
			if !validRepositoryURL(r.URL) {
				return doc, fmt.Errorf("repository %d must use HTTPS or SSH without embedded passwords", i+1)
			}
			if r.Path == "" {
				r.Path = fmt.Sprintf("repo-%d", i+1)
			}
			if !safeRelativePath(r.Path) || r.Path == ".codex" || strings.HasPrefix(r.Path, ".codex/") || strings.HasPrefix(r.Ref, "-") || strings.ContainsAny(r.Ref, "\r\n") {
				return doc, errors.New("repository path must be relative, outside .codex, and ref must not start with a dash")
			}
			for existing := range paths {
				if existing == r.Path || strings.HasPrefix(existing, r.Path+"/") || strings.HasPrefix(r.Path, existing+"/") {
					return doc, errors.New("repository paths must not overlap")
				}
			}
			paths[r.Path] = true
		}
		for name := range s.Env {
			if !envPattern.MatchString(name) {
				return doc, fmt.Errorf("invalid environment variable %q", name)
			}
		}
		for name, ref := range s.SecretEnv {
			if !envPattern.MatchString(name) || !envPattern.MatchString(ref) {
				return doc, errors.New("secretEnv entries must map environment variable names to secret variable names")
			}
		}
		for name, server := range s.MCPServers {
			if !namePattern.MatchString(name) {
				return doc, fmt.Errorf("invalid MCP server name %q", name)
			}
			if (server.Command == "") == (server.URL == "") {
				return doc, fmt.Errorf("MCP server %s requires exactly one of command or url", name)
			}
			if server.URL != "" && !validHTTPURL(server.URL) {
				return doc, fmt.Errorf("MCP server %s url must use HTTP(S)", name)
			}
			for env := range server.Env {
				if !envPattern.MatchString(env) {
					return doc, fmt.Errorf("invalid MCP environment variable %q", env)
				}
			}
		}
		spec = s
	case "Runner":
		var s model.RunnerSpec
		if err := decodeStrict(doc.Spec, &s); err != nil {
			return doc, err
		}
		if s.Type == "" {
			s.Type = "docker"
		}
		if s.Type != "docker" {
			return doc, errors.New("runner type must be docker")
		}
		if s.Image == "" {
			s.Image = "cloudpilot-codex:latest"
		}
		for name, value := range map[string]string{"image": s.Image, "network": s.Network, "memory": s.Memory, "cpus": s.CPUs} {
			if strings.HasPrefix(value, "-") || strings.ContainsAny(value, "\r\n\x00") {
				return doc, fmt.Errorf("invalid runner %s", name)
			}
		}
		spec = s
	default:
		return doc, errors.New("kind must be Input, Pipeline, Worker, Workspace or Runner")
	}
	var err error
	doc.Spec, err = json.Marshal(spec)
	return doc, err
}

func references(doc model.Document) []string {
	switch doc.Kind {
	case "Input":
		var s model.InputSpec
		if json.Unmarshal(doc.Spec, &s) == nil {
			return []string{key("Pipeline", s.PipelineRef)}
		}
	case "Pipeline":
		var s model.PipelineSpec
		if json.Unmarshal(doc.Spec, &s) == nil {
			return []string{key("Worker", s.WorkerRef), key("Workspace", s.WorkspaceRef), key("Runner", s.RunnerRef)}
		}
	}
	return nil
}

func safeRelativePath(value string) bool {
	return value != "" && value != "." && !strings.HasPrefix(value, "/") && !strings.Contains(value, "\\") && path.Clean(value) == value && value != ".." && !strings.HasPrefix(value, "../") && !strings.ContainsAny(value, "\x00\r\n")
}

func validHTTPURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil
}

func validRepositoryURL(value string) bool {
	if strings.HasPrefix(value, "git@") && strings.Contains(value, ":") && !strings.ContainsAny(value, " \r\n\x00") {
		return true
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "ssh") {
		return false
	}
	if u.User != nil {
		_, hasPassword := u.User.Password()
		return !hasPassword && u.Scheme == "ssh"
	}
	return true
}
