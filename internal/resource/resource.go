package resource

import (
	"bytes"
	"crypto/cipher"
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
	mutation           sync.Mutex
	namePattern        = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	envPattern         = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	githubOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	githubRepoPattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	ErrReferenced      = errors.New("resource is still referenced")
	ErrConflict        = errors.New("resource changed; reload before saving")
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
		doc.Spec = (model.Resource{Kind: doc.Kind, Spec: doc.Spec}).Document().Spec
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
func Apply(db *gorm.DB, docs []model.Document, encryption cipher.AEAD) ([]model.Document, error) {
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
			graph[key(item.Kind, item.Name)] = model.Document{APIVersion: apiVersion, Kind: item.Kind, Metadata: model.Metadata{Name: item.Name, Generation: item.Generation}, Spec: item.Spec}
		}
		batch := make(map[string]bool, len(docs))
		for _, original := range docs {
			doc, err := normalize(original, graph[key(original.Kind, original.Metadata.Name)], encryption)
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

// validateGitHubInput requires an explicit repository and at least one trigger filter.
func validateGitHubInput(s *model.GitHubInputSpec) error {
	if s == nil {
		return errors.New("github configuration is required for github_issue inputs")
	}
	owner, repo, ok := strings.Cut(s.Repository, "/")
	if !ok || !githubOwnerPattern.MatchString(owner) || !githubRepoPattern.MatchString(repo) || repo == "." || repo == ".." {
		return errors.New("github.repository must be an owner/repo name")
	}
	if !envPattern.MatchString(s.TokenSecretRef) {
		return errors.New("github.tokenSecretRef must name an environment variable")
	}
	if len(s.Assignees)+len(s.Labels) == 0 {
		return errors.New("github.assignees or github.labels must contain at least one trigger filter")
	}
	for _, assignee := range s.Assignees {
		if assignee != "*" && !githubOwnerPattern.MatchString(assignee) {
			return errors.New("github.assignees must contain GitHub usernames or * for any assignee")
		}
	}
	for _, label := range s.Labels {
		if strings.TrimSpace(label) == "" || len([]rune(label)) > 50 || strings.ContainsAny(label, "\r\n") {
			return errors.New("github.labels must contain nonblank, single-line labels of at most 50 characters")
		}
	}
	return nil
}

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

func normalize(doc, previous model.Document, encryption cipher.AEAD) (model.Document, error) {
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
		if s.Type != "webhook" && s.Type != "github_issue" {
			return doc, errors.New("input type must be webhook or github_issue")
		}
		if !namePattern.MatchString(s.PipelineRef) || !envPattern.MatchString(s.SecretRef) {
			return doc, errors.New("pipelineRef and secretRef (environment variable name) are required")
		}
		if s.Type == "github_issue" {
			if err := validateGitHubInput(s.GitHub); err != nil {
				return doc, err
			}
		} else if s.GitHub != nil {
			return doc, errors.New("github configuration requires input type github_issue")
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
		var old model.WorkerSpec
		if len(previous.Spec) > 0 {
			if err := json.Unmarshal(previous.Spec, &old); err != nil {
				return doc, err
			}
		}
		var err error
		s.APIKey, err = sealAPIKey(encryption, doc.Kind, s.APIKey, s.APIKeySecretRef, old.APIKey)
		if err != nil {
			return doc, err
		}
		if s.APIKey != "" {
			s.APIKeySecretRef = ""
		} else if !envPattern.MatchString(s.APIKeySecretRef) {
			return doc, errors.New("apiKey is required; apiKeySecretRef may reference a legacy environment variable")
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
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(doc.Spec, &fields); err != nil {
			return doc, err
		}
		switch s.Type {
		case "docker":
			if _, ok := fields["e2b"]; ok {
				return doc, errors.New("docker runner must not include e2b configuration")
			}
			if s.Image == "" {
				s.Image = "cloudpilot-codex:latest"
			}
			for name, value := range map[string]string{"image": s.Image, "network": s.Network, "memory": s.Memory, "cpus": s.CPUs} {
				if strings.HasPrefix(value, "-") || strings.ContainsAny(value, "\r\n\x00") {
					return doc, fmt.Errorf("invalid runner %s", name)
				}
			}
		case "e2b":
			for _, name := range []string{"image", "network", "memory", "cpus"} {
				if _, ok := fields[name]; ok {
					return doc, fmt.Errorf("e2b runner must not include docker field %s", name)
				}
			}
			var old model.RunnerSpec
			if len(previous.Spec) > 0 {
				if err := json.Unmarshal(previous.Spec, &old); err != nil {
					return doc, err
				}
			}
			if s.E2B != nil {
				var previousKey string
				if old.E2B != nil {
					previousKey = old.E2B.APIKey
				}
				var err error
				s.E2B.APIKey, err = sealAPIKey(encryption, doc.Kind, s.E2B.APIKey, s.E2B.APIKeySecretRef, previousKey)
				if err != nil {
					return doc, err
				}
				if s.E2B.APIKey != "" {
					s.E2B.APIKeySecretRef = ""
				}
			}
			if err := normalizeE2B(s.E2B); err != nil {
				return doc, err
			}
		default:
			return doc, errors.New("runner type must be docker or e2b")
		}
		spec = s
	default:
		return doc, errors.New("kind must be Input, Pipeline, Worker, Workspace or Runner")
	}
	var err error
	doc.Spec, err = json.Marshal(spec)
	return doc, err
}

// normalizeE2B validates the provider connection and applies portable runner defaults.
func normalizeE2B(s *model.E2BRunnerSpec) error {
	if s == nil {
		return errors.New("e2b runner requires e2b configuration")
	}
	if s.Provider == "" {
		s.Provider = "e2b"
	}
	if s.Provider != "e2b" && s.Provider != "novita" {
		return errors.New("e2b provider must be e2b or novita")
	}
	if s.Domain == "" {
		s.Domain = "e2b.app"
		if s.Provider == "novita" {
			s.Domain = "us-phx-1.sandbox.novita.ai"
		}
	}
	if len(s.Domain) > 253 {
		return errors.New("e2b domain must be a DNS hostname without a scheme, port or path")
	}
	for label := range strings.SplitSeq(s.Domain, ".") {
		if !namePattern.MatchString(strings.ToLower(label)) {
			return errors.New("e2b domain must be a DNS hostname without a scheme, port or path")
		}
	}
	if s.APIURL != "" {
		u, err := url.Parse(s.APIURL)
		if err != nil || !validHTTPURL(s.APIURL) || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("e2b apiUrl must be an absolute HTTP(S) URL without embedded credentials, query or fragment")
		}
	}
	if s.APIKey == "" && !envPattern.MatchString(s.APIKeySecretRef) {
		return errors.New("e2b apiKey is required; apiKeySecretRef may reference a legacy environment variable")
	}
	if strings.TrimSpace(s.Template) == "" || len(s.Template) > 255 || strings.ContainsAny(s.Template, " \t\r\n\x00") {
		return errors.New("e2b template must name a sandbox template with Node.js and Codex installed")
	}
	if s.LeaseSeconds == 0 {
		s.LeaseSeconds = 900
	}
	if s.LeaseSeconds < 60 || s.LeaseSeconds > 86400 {
		return errors.New("e2b leaseSeconds must be between 60 and 86400")
	}
	if s.IdleStrategy == "" {
		s.IdleStrategy = "pause"
	}
	if s.IdleStrategy != "pause" {
		return errors.New("e2b idleStrategy must be pause")
	}
	return nil
}

// sealAPIKey encrypts a replacement or preserves the stored credential on an ordinary edit.
func sealAPIKey(encryption cipher.AEAD, purpose, value, reference, previous string) (string, error) {
	if value == "" {
		if reference != "" {
			return "", nil
		}
		return previous, nil
	}
	if strings.TrimSpace(value) == "" || len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("apiKey must contain 1–4096 bytes without newlines or NUL")
	}
	return model.Encrypt(encryption, purpose, value)
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
