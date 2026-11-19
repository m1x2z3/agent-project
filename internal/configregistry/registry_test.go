package configregistry

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestRegistryPublishesAndActivatesImmutableSnapshot(t *testing.T) {
	registry := NewRegistry()
	snapshot := testSnapshot("v1", time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))

	if err := registry.Publish(snapshot); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if _, err := registry.Active(); !errors.Is(err, ErrNoActiveVersion) {
		t.Fatalf("Active() error = %v, want ErrNoActiveVersion", err)
	}
	if err := registry.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}

	active, err := registry.Active()
	if err != nil {
		t.Fatalf("Active() error = %v", err)
	}
	active.Prompts["main"] = PromptVersion{Version: "tampered"}
	active.Routes["chat"] = ModelRouteVersion{Model: "tampered"}

	again, err := registry.Get("v1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if again.Prompts["main"].Version != "prompt.v1" ||
		again.Routes["chat"].Model != "deepseek-chat" {
		t.Fatalf("registry was modified through returned snapshot: %+v", again)
	}
}

func TestRegistryRejectsInvalidAndDuplicateVersions(t *testing.T) {
	registry := NewRegistry()
	tests := []struct {
		name  string
		apply func(*ConfigSnapshot)
	}{
		{
			name: "missing release version",
			apply: func(snapshot *ConfigSnapshot) {
				snapshot.ReleaseVersion = ""
			},
		},
		{
			name: "metadata version mismatch",
			apply: func(snapshot *ConfigSnapshot) {
				snapshot.Metadata.Version = "other"
			},
		},
		{
			name: "missing prompt",
			apply: func(snapshot *ConfigSnapshot) {
				snapshot.Prompts = nil
			},
		},
		{
			name: "missing schema version",
			apply: func(snapshot *ConfigSnapshot) {
				snapshot.Schemas["task_analysis"] = SchemaVersion{Kind: "task_analysis"}
			},
		},
		{
			name: "invalid model route",
			apply: func(snapshot *ConfigSnapshot) {
				snapshot.Routes["chat"] = ModelRouteVersion{Task: "chat", Version: "route.v1", Provider: "deepseek", Model: "deepseek-chat", Timeout: -time.Second}
			},
		},
		{
			name: "invalid pricing",
			apply: func(snapshot *ConfigSnapshot) {
				pricing := snapshot.Pricing["deepseek-chat"]
				pricing.InputPer1K = -1
				snapshot.Pricing["deepseek-chat"] = pricing
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := testSnapshot("v1", time.Now())
			test.apply(&snapshot)
			if err := registry.Publish(snapshot); !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("Publish() error = %v, want ErrInvalidSnapshot", err)
			}
		})
	}

	snapshot := testSnapshot("v1", time.Now())
	if err := registry.Publish(snapshot); err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	if err := registry.Publish(snapshot); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("duplicate Publish() error = %v, want ErrVersionExists", err)
	}
	if err := registry.Activate("missing"); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("unknown Activate() error = %v, want ErrVersionNotFound", err)
	}
}

func TestRegistryHistoryAndActiveVersionSwitch(t *testing.T) {
	registry := NewRegistry()
	v1 := testSnapshot("v1", time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	v2 := testSnapshot("v2", time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC))
	if err := registry.Publish(v1); err != nil {
		t.Fatalf("Publish(v1) error = %v", err)
	}
	if err := registry.Publish(v2); err != nil {
		t.Fatalf("Publish(v2) error = %v", err)
	}
	if err := registry.Activate("v1"); err != nil {
		t.Fatalf("Activate(v1) error = %v", err)
	}
	if err := registry.Activate("v2"); err != nil {
		t.Fatalf("Activate(v2) error = %v", err)
	}

	active, err := registry.Active()
	if err != nil {
		t.Fatalf("Active() error = %v", err)
	}
	if active.ReleaseVersion != "v2" {
		t.Fatalf("active version = %q, want v2", active.ReleaseVersion)
	}

	history := registry.History()
	if len(history) != 2 || history[0].Version != "v2" || history[1].Version != "v1" {
		t.Fatalf("history = %+v, want v2 then v1", history)
	}
	history[0].Version = "tampered"
	if registry.History()[0].Version != "v2" {
		t.Fatal("history result modified registry state")
	}
}

func TestRegistryRollbackCreatesNewActiveVersion(t *testing.T) {
	registry := NewRegistry()
	v1 := testSnapshot("v1", time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	v2 := testSnapshot("v2", time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC))
	if err := registry.Publish(v1); err != nil {
		t.Fatalf("Publish(v1) error = %v", err)
	}
	if err := registry.Publish(v2); err != nil {
		t.Fatalf("Publish(v2) error = %v", err)
	}
	if err := registry.Activate("v2"); err != nil {
		t.Fatalf("Activate(v2) error = %v", err)
	}

	rollback, err := registry.Rollback("v1", "v3", "release-bot", time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if rollback.ReleaseVersion != "v3" ||
		rollback.Metadata.Version != "v3" ||
		rollback.Metadata.PreviousVersion != "v2" ||
		rollback.Metadata.RollbackFrom != "v1" {
		t.Fatalf("rollback metadata = %+v", rollback.Metadata)
	}

	active, err := registry.Active()
	if err != nil {
		t.Fatalf("Active() error = %v", err)
	}
	if active.ReleaseVersion != "v3" || active.Prompts["main"].Version != "prompt.v1" {
		t.Fatalf("active rollback snapshot = %+v", active)
	}

	original, err := registry.Get("v1")
	if err != nil {
		t.Fatalf("Get(v1) error = %v", err)
	}
	if original.ReleaseVersion != "v1" || original.Metadata.RollbackFrom != "" {
		t.Fatalf("original version was modified: %+v", original.Metadata)
	}
}

func TestRegistryBindsRunToHistoricalSnapshot(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Publish(testSnapshot("v1", time.Now())); err != nil {
		t.Fatalf("Publish(v1) error = %v", err)
	}
	if err := registry.Publish(testSnapshot("v2", time.Now().Add(time.Second))); err != nil {
		t.Fatalf("Publish(v2) error = %v", err)
	}
	if err := registry.Activate("v1"); err != nil {
		t.Fatalf("Activate(v1) error = %v", err)
	}

	bound, err := registry.BindRun("run-1")
	if err != nil {
		t.Fatalf("BindRun() error = %v", err)
	}
	if bound.ReleaseVersion != "v1" {
		t.Fatalf("bound version = %q, want v1", bound.ReleaseVersion)
	}

	if err := registry.Activate("v2"); err != nil {
		t.Fatalf("Activate(v2) error = %v", err)
	}
	original, err := registry.GetRunSnapshot("run-1")
	if err != nil {
		t.Fatalf("GetRunSnapshot() error = %v", err)
	}
	if original.ReleaseVersion != "v1" {
		t.Fatalf("run version changed after activation: %q", original.ReleaseVersion)
	}

	original.Snapshot.Prompts["main"] = PromptVersion{Version: "tampered"}
	stored, err := registry.GetRunSnapshot("run-1")
	if err != nil {
		t.Fatalf("GetRunSnapshot() second error = %v", err)
	}
	if stored.Snapshot.Prompts["main"].Version != "prompt.v1" {
		t.Fatal("run snapshot was modified through returned value")
	}
	if _, err := registry.BindRun("run-1"); !errors.Is(err, ErrRunAlreadyBound) {
		t.Fatalf("duplicate BindRun() error = %v, want ErrRunAlreadyBound", err)
	}
	if _, err := registry.GetRunSnapshot("missing"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("unknown GetRunSnapshot() error = %v, want ErrRunNotFound", err)
	}
}

func TestRegistryBindsRunToSpecifiedPublishedVersion(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Publish(testSnapshot("v1", time.Now())); err != nil {
		t.Fatalf("Publish(v1) error = %v", err)
	}
	if err := registry.Publish(testSnapshot("v2", time.Now().Add(time.Second))); err != nil {
		t.Fatalf("Publish(v2) error = %v", err)
	}
	if err := registry.Activate("v1"); err != nil {
		t.Fatalf("Activate(v1) error = %v", err)
	}

	bound, err := registry.BindRunToVersion("run-candidate", "v2")
	if err != nil {
		t.Fatalf("BindRunToVersion() error = %v", err)
	}
	if bound.ReleaseVersion != "v2" {
		t.Fatalf("bound version = %q, want v2", bound.ReleaseVersion)
	}
	if _, err := registry.BindRunToVersion("run-candidate", "v1"); !errors.Is(err, ErrRunAlreadyBound) {
		t.Fatalf("duplicate BindRunToVersion() error = %v, want ErrRunAlreadyBound", err)
	}
	if _, err := registry.BindRunToVersion("run-missing", "missing"); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("unknown version error = %v, want ErrVersionNotFound", err)
	}
}

func TestRegistryResolvesStructuredOutputVersionReferences(t *testing.T) {
	snapshot := testSnapshot("v1", time.Now())
	references, err := snapshot.ResolveVersionReferences("main", "task_analysis", "chat")
	if err != nil {
		t.Fatalf("ResolveVersionReferences() error = %v", err)
	}
	want := VersionReferences{
		Provider:           "deepseek",
		Model:              "deepseek-chat",
		PromptVersion:      "prompt.v1",
		SchemaVersion:      "task_analysis.v1",
		ModelConfigVersion: "route.v1",
	}
	if !reflect.DeepEqual(references, want) {
		t.Fatalf("references = %+v, want %+v", references, want)
	}
	if _, err := snapshot.ResolveVersionReferences("missing", "task_analysis", "chat"); !errors.Is(err, ErrVersionReferenceNotFound) {
		t.Fatalf("missing reference error = %v, want ErrVersionReferenceNotFound", err)
	}
}

func TestRegistryDoesNotStoreAPIKeys(t *testing.T) {
	field, exists := reflect.TypeOf(ModelRouteVersion{}).FieldByName("APIKey")
	if exists {
		t.Fatalf("ModelRouteVersion unexpectedly contains sensitive field: %+v", field)
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Publish(testSnapshot("v0", time.Now())); err != nil {
		t.Fatalf("Publish(v0) error = %v", err)
	}
	if err := registry.Activate("v0"); err != nil {
		t.Fatalf("Activate(v0) error = %v", err)
	}
	for i := 1; i <= 20; i++ {
		version := "v" + string(rune('a'+i))
		if err := registry.Publish(testSnapshot(version, time.Now().Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatalf("Publish(%s) error = %v", version, err)
		}
	}

	var group sync.WaitGroup
	errs := make(chan error, 60)
	for i := 1; i <= 20; i++ {
		version := "v" + string(rune('a'+i))
		group.Add(1)
		go func(version string) {
			defer group.Done()
			if err := registry.Activate(version); err != nil {
				errs <- err
				return
			}
			if _, err := registry.Get(version); err != nil {
				errs <- err
			}
			_ = registry.History()
			_, _ = registry.Active()
		}(version)
	}
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			_, err := registry.BindRun("run-" + string(rune('a'+index)))
			if err != nil {
				errs <- err
			}
		}(i)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent registry operation error = %v", err)
	}
}

func testSnapshot(version string, publishedAt time.Time) ConfigSnapshot {
	return ConfigSnapshot{
		ReleaseVersion: version,
		Prompts: map[string]PromptVersion{
			"main": {
				Name:     "main",
				Version:  "prompt.v1",
				Template: "研发问题：{query}",
			},
		},
		Schemas: map[string]SchemaVersion{
			"task_analysis": {
				Kind:    "task_analysis",
				Version: "task_analysis.v1",
			},
		},
		Routes: map[string]ModelRouteVersion{
			"chat": {
				Task:        "chat",
				Version:     "route.v1",
				Provider:    "deepseek",
				Model:       "deepseek-chat",
				Temperature: 0.2,
				MaxTokens:   512,
				Timeout:     30 * time.Second,
			},
		},
		Pricing: map[string]PricingVersion{
			"deepseek-chat": {
				Provider:    "deepseek",
				Model:       "deepseek-chat",
				Version:     "price.v1",
				Currency:    "USD",
				InputPer1K:  0.001,
				OutputPer1K: 0.002,
			},
		},
		Metadata: ReleaseMetadata{
			Version:     version,
			PublishedBy: "test",
			PublishedAt: publishedAt,
			EffectiveAt: publishedAt,
		},
	}
}
