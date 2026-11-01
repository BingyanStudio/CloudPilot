package resource

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BingyanStudio/CloudPilot/internal/model"
)

// TestEncryptedAPIKeys covers direct credentials, redaction, ordinary edits and replacement.
func TestEncryptedAPIKeys(t *testing.T) {
	db, err := model.Open("sqlite", filepath.Join(t.TempDir(), "credentials.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	encryption, err := model.Unlock(db, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32))))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ kind, spec string }{
		{"Worker", `{"model":"fixture","apiKey":"private-api-key"}`},
		{"Runner", `{"type":"e2b","e2b":{"template":"fixture","apiKey":"private-api-key"}}`},
	} {
		doc := model.Document{APIVersion: model.APIVersion, Kind: tc.kind, Metadata: model.Metadata{Name: "encrypted"}, Spec: json.RawMessage(tc.spec)}
		applied, err := Apply(db, []model.Document{doc}, encryption)
		if err != nil {
			t.Fatal(err)
		}
		readKey := func() string {
			t.Helper()
			stored, err := model.FindResource(db, tc.kind, "encrypted")
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := stored.Decode(&fields); err != nil {
				t.Fatal(err)
			}
			if tc.kind == "Runner" {
				fields = fields["e2b"].(map[string]any)
			}
			return fields["apiKey"].(string)
		}
		sealed := readKey()
		if value, err := model.Decrypt(encryption, tc.kind, sealed); err != nil || value != "private-api-key" {
			t.Fatal("credential was not encrypted", err)
		}
		for _, docs := range [][]model.Document{applied, {doc}} {
			exported, err := ExportYAML(docs)
			if err != nil || strings.Contains(string(exported), "apiKey:") || strings.Contains(string(exported), sealed) {
				t.Fatal("export exposed a credential", err)
			}
		}
		if strings.Contains(string(applied[0].Spec), "apiKey") {
			t.Fatal("API response exposed the credential field")
		}
		applied, err = Apply(db, applied, encryption)
		if err != nil || readKey() != sealed {
			t.Fatal("ordinary edit did not preserve the stored credential", err)
		}
		replacement := applied[0]
		replacement.Spec = json.RawMessage(strings.ReplaceAll(tc.spec, "private-api-key", "replacement-api-key"))
		if _, err := Apply(db, []model.Document{replacement}, encryption); err != nil {
			t.Fatal(err)
		}
		if value, err := model.Decrypt(encryption, tc.kind, readKey()); err != nil || value != "replacement-api-key" {
			t.Fatal("credential replacement failed", err)
		}
	}
	for _, value := range []string{"", "\nsecret", strings.Repeat("x", 4097)} {
		spec, _ := json.Marshal(model.WorkerSpec{Model: "fixture", APIKey: value})
		doc := model.Document{APIVersion: model.APIVersion, Kind: "Worker", Metadata: model.Metadata{Name: "invalid"}, Spec: spec}
		if _, err := Apply(db, []model.Document{doc}, encryption); err == nil {
			t.Fatal("accepted a missing or invalid API key")
		}
	}
}

const testManifest = `apiVersion: cloudpilot.dev/v1alpha1
kind: Input
metadata: {name: feedback}
spec: {pipelineRef: nightly, secretRef: FEEDBACK_TOKEN}
---
apiVersion: cloudpilot.dev/v1alpha1
kind: Pipeline
metadata: {name: nightly}
spec: {workerRef: codex, workspaceRef: app, runnerRef: docker, schedule: "0 20 * * *"}
---
apiVersion: cloudpilot.dev/v1alpha1
kind: Worker
metadata: {name: codex}
spec: {model: gpt-5, apiKeySecretRef: OPENAI_API_KEY}
---
apiVersion: cloudpilot.dev/v1alpha1
kind: Workspace
metadata: {name: app}
spec: {repositories: [{url: "https://github.com/example/app.git", path: app}]}
---
apiVersion: cloudpilot.dev/v1alpha1
kind: Runner
metadata: {name: docker}
spec: {}
`

func TestManifestApplyRoundTripAndAtomicValidation(t *testing.T) {
	db, err := model.Open("sqlite", filepath.Join(t.TempDir(), "cloudpilot.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	docs, err := ParseYAML([]byte(testManifest))
	if err != nil {
		t.Fatal(err)
	}
	applied, err := Apply(db, docs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 5 {
		t.Fatalf("got %d resources", len(applied))
	}
	pipeline, err := model.FindResource(db, "Pipeline", "nightly")
	if err != nil {
		t.Fatal(err)
	}
	var spec model.PipelineSpec
	if err := pipeline.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	if spec.Timezone != "Asia/Shanghai" || spec.Concurrency != 1 || spec.TimeoutSeconds != 3600 {
		t.Fatalf("defaults not applied: %+v", spec)
	}
	exported, err := ExportYAML(applied)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(exported), "generation:") {
		t.Fatal("export leaked persistence generation")
	}
	roundTrip, err := ParseYAML(exported)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Apply(db, roundTrip, nil); err != nil {
		t.Fatalf("round-trip import: %v", err)
	}
	if _, err = Apply(db, applied, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update should conflict: %v", err)
	}
	if err = Delete(db, "Worker", "codex"); !errors.Is(err, ErrReferenced) {
		t.Fatalf("referenced delete: %v", err)
	}

	bad := []model.Document{{APIVersion: model.APIVersion, Kind: "Runner", Metadata: model.Metadata{Name: "rollback"}, Spec: json.RawMessage(`{}`)}, {APIVersion: model.APIVersion, Kind: "Input", Metadata: model.Metadata{Name: "bad"}, Spec: json.RawMessage(`{"pipelineRef":"missing","secretRef":"SECRET"}`)}}
	if _, err = Apply(db, bad, nil); err == nil {
		t.Fatal("missing reference was accepted")
	}
	var count int64
	db.Model(&model.Resource{}).Where("name IN ?", []string{"rollback", "bad"}).Count(&count)
	if count != 0 {
		t.Fatal("failed batch partially persisted")
	}

	for _, input := range []string{
		strings.Replace(testManifest, "pipelineRef: nightly", "pipelineRef: nightly, typo: ignored", 1),
		strings.Replace(testManifest, "metadata: {name: feedback}", "metadata: {name: feedback, labels: invalid}", 1),
		strings.Replace(testManifest, "path: app", "path: ../outside", 1),
		strings.Replace(testManifest, "path: app", "path: .codex/tools", 1),
		strings.Replace(testManifest, `schedule: "0 20 * * *"`, `schedule: "every night"`, 1),
		strings.Replace(testManifest, "secretRef: FEEDBACK_TOKEN", "secretRef: my-secret-value", 1),
	} {
		parsed, parseErr := ParseYAML([]byte(input))
		if parseErr == nil {
			_, parseErr = Apply(db, parsed, nil)
		}
		if parseErr == nil {
			t.Errorf("invalid manifest was accepted: %s", input)
		}
	}
	if _, err = ParseYAML([]byte("kind: Input\nkind: Worker")); err == nil {
		t.Fatal("duplicate YAML key was accepted")
	}
	if err := Delete(db, "Input", "feedback"); err != nil {
		t.Fatal(err)
	}
	run := model.Run{PipelineName: "nightly", Status: "succeeded"}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	session := model.AgentSession{RunID: run.ID, State: "idle"}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	if err := Delete(db, "Pipeline", "nightly"); !errors.Is(err, ErrReferenced) {
		t.Fatalf("open session pipeline was removed: %v", err)
	}
	if err := db.Model(&session).Update("state", "closed").Error; err != nil {
		t.Fatal(err)
	}
	if err := Delete(db, "Pipeline", "nightly"); err != nil {
		t.Fatalf("closed session should not block pipeline deletion: %v", err)
	}
}

func TestE2BRunnerConfiguration(t *testing.T) {
	runner := func(spec string) model.Document {
		return model.Document{APIVersion: model.APIVersion, Kind: "Runner", Metadata: model.Metadata{Name: "sandbox"}, Spec: json.RawMessage(spec)}
	}
	for _, provider := range []struct{ name, domain string }{{"e2b", "e2b.app"}, {"novita", "us-phx-1.sandbox.novita.ai"}} {
		t.Run(provider.name, func(t *testing.T) {
			input := `{"type":"e2b","e2b":{"provider":"` + provider.name + `","apiKeySecretRef":"SANDBOX_API_KEY","template":"cloudpilot-codex"}}`
			doc, err := normalize(runner(input), model.Document{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var spec model.RunnerSpec
			if err := json.Unmarshal(doc.Spec, &spec); err != nil {
				t.Fatal(err)
			}
			if spec.E2B == nil || spec.E2B.Domain != provider.domain || spec.E2B.LeaseSeconds != 900 || spec.E2B.IdleStrategy != "pause" || spec.Image != "" {
				t.Fatalf("unexpected runner defaults: %s", doc.Spec)
			}
			data, err := ExportYAML([]model.Document{doc})
			if err != nil {
				t.Fatal(err)
			}
			docs, err := ParseYAML(data)
			if err != nil || len(docs) != 1 {
				t.Fatalf("parse exported runner: %v", err)
			}
			if _, err := normalize(docs[0], model.Document{}, nil); err != nil {
				t.Fatalf("round-trip runner configuration: %v", err)
			}
		})
	}
	valid := `{"type":"e2b","e2b":{"apiKeySecretRef":"SANDBOX_API_KEY","template":"cloudpilot-codex"}}`
	for _, input := range []string{
		`{"type":"e2b"}`,
		`{"type":"docker","e2b":null}`,
		strings.Replace(valid, `"type":"e2b"`, `"type":"docker"`, 1),
		strings.Replace(valid, `"type":"e2b"`, `"type":"e2b","image":""`, 1),
		strings.Replace(valid, `"type":"e2b"`, `"type":"e2b","cpus":"2"`, 1),
		strings.Replace(valid, `"template":"cloudpilot-codex"`, `"template":""`, 1),
		strings.Replace(valid, `"SANDBOX_API_KEY"`, `"secret-value"`, 1),
		strings.Replace(valid, `"e2b":{`, `"e2b":{"provider":"unsupported",`, 1),
		strings.Replace(valid, `"e2b":{`, `"e2b":{"domain":"https://sandbox.example.com",`, 1),
		strings.Replace(valid, `"e2b":{`, `"e2b":{"domain":"sandbox.example.com:443",`, 1),
		strings.Replace(valid, `"e2b":{`, `"e2b":{"apiUrl":"https://key@api.example.com",`, 1),
		strings.Replace(valid, `"e2b":{`, `"e2b":{"apiUrl":"https://api.example.com?key=secret",`, 1),
		strings.Replace(valid, `"e2b":{`, `"e2b":{"leaseSeconds":30,`, 1),
		strings.Replace(valid, `"e2b":{`, `"e2b":{"leaseSeconds":86401,`, 1),
		strings.Replace(valid, `"e2b":{`, `"e2b":{"idleStrategy":"keepalive",`, 1),
		strings.Replace(valid, `"e2b":{`, `"e2b":{"typo":true,`, 1),
	} {
		if _, err := normalize(runner(input), model.Document{}, nil); err == nil {
			t.Errorf("invalid runner configuration accepted: %s", input)
		}
	}
	custom := strings.Replace(valid, `"e2b":{`, `"e2b":{"domain":"sandboxes.example.com","apiUrl":"https://api.example.com/v1","leaseSeconds":1800,`, 1)
	doc, err := normalize(runner(custom), model.Document{}, nil)
	if err != nil {
		t.Fatalf("custom provider endpoint rejected: %v", err)
	}
	var spec model.RunnerSpec
	if err := json.Unmarshal(doc.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.E2B.Provider != "e2b" || spec.E2B.Domain != "sandboxes.example.com" || spec.E2B.APIURL != "https://api.example.com/v1" || spec.E2B.LeaseSeconds != 1800 {
		t.Fatalf("custom connection changed: %s", doc.Spec)
	}
}

func TestGitHubInputValidationAndRoundTrip(t *testing.T) {
	db, err := model.Open("sqlite", filepath.Join(t.TempDir(), "github.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	manifest := strings.Replace(testManifest, "spec: {pipelineRef: nightly, secretRef: FEEDBACK_TOKEN}", `spec:
  type: github_issue
  pipelineRef: nightly
  secretRef: FEEDBACK_TOKEN
  github:
    repository: example/app
    tokenSecretRef: GITHUB_TOKEN
    assignees: [pilot, '*']
    labels: [agent, 'help wanted']`, 1)
	docs, err := ParseYAML([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	applied, err := Apply(db, docs, nil)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := ExportYAML(applied)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := ParseYAML(exported)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(db, roundTrip, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := model.FindResource(db, "Input", "feedback")
	if err != nil {
		t.Fatal(err)
	}
	var spec model.InputSpec
	if err := stored.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	if spec.GitHub == nil || spec.GitHub.Repository != "example/app" || spec.GitHub.TokenSecretRef != "GITHUB_TOKEN" || strings.Join(spec.GitHub.Assignees, ",") != "pilot,*" || strings.Join(spec.GitHub.Labels, ",") != "agent,help wanted" {
		t.Fatalf("GitHub configuration lost on round trip: %+v", spec.GitHub)
	}
	for _, tc := range []struct {
		name string
		edit func(*model.InputSpec)
	}{
		{"missing settings", func(s *model.InputSpec) { s.GitHub = nil }},
		{"unsupported type", func(s *model.InputSpec) { s.Type = "github" }},
		{"webhook settings", func(s *model.InputSpec) { s.Type = "webhook" }},
		{"repository URL", func(s *model.InputSpec) { s.GitHub.Repository = "https://github.com/example/app" }},
		{"repository traversal", func(s *model.InputSpec) { s.GitHub.Repository = "example/.." }},
		{"missing token reference", func(s *model.InputSpec) { s.GitHub.TokenSecretRef = "" }},
		{"no filters", func(s *model.InputSpec) { s.GitHub.Assignees, s.GitHub.Labels = nil, nil }},
		{"invalid assignee", func(s *model.InputSpec) { s.GitHub.Assignees = []string{"@pilot"} }},
		{"blank label", func(s *model.InputSpec) { s.GitHub.Labels = []string{" "} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var invalid model.InputSpec
			if err := stored.Decode(&invalid); err != nil {
				t.Fatal(err)
			}
			tc.edit(&invalid)
			body, err := json.Marshal(invalid)
			if err != nil {
				t.Fatal(err)
			}
			doc := stored.Document()
			doc.Spec = body
			if _, err := Apply(db, []model.Document{doc}, nil); err == nil {
				t.Fatal("invalid GitHub input accepted")
			}
		})
	}
}
