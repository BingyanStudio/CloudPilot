package resource

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BingyanStudio/CloudPilot/internal/model"
)

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
	applied, err := Apply(db, docs)
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
	if _, err = Apply(db, roundTrip); err != nil {
		t.Fatalf("round-trip import: %v", err)
	}
	if _, err = Apply(db, applied); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update should conflict: %v", err)
	}
	if err = Delete(db, "Worker", "codex"); !errors.Is(err, ErrReferenced) {
		t.Fatalf("referenced delete: %v", err)
	}

	bad := []model.Document{{APIVersion: model.APIVersion, Kind: "Runner", Metadata: model.Metadata{Name: "rollback"}, Spec: json.RawMessage(`{}`)}, {APIVersion: model.APIVersion, Kind: "Input", Metadata: model.Metadata{Name: "bad"}, Spec: json.RawMessage(`{"pipelineRef":"missing","secretRef":"SECRET"}`)}}
	if _, err = Apply(db, bad); err == nil {
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
			_, parseErr = Apply(db, parsed)
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
			doc, err := normalize(runner(input))
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
			if _, err := normalize(docs[0]); err != nil {
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
		if _, err := normalize(runner(input)); err == nil {
			t.Errorf("invalid runner configuration accepted: %s", input)
		}
	}
	custom := strings.Replace(valid, `"e2b":{`, `"e2b":{"domain":"sandboxes.example.com","apiUrl":"https://api.example.com/v1","leaseSeconds":1800,`, 1)
	doc, err := normalize(runner(custom))
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
