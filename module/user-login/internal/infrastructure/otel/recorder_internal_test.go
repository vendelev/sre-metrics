package otel

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestNewResourceUUIDError(t *testing.T) {
	cause := errors.New("синтетический сбой генерации UUID")
	res, err := newResource(
		ResourceSettings{NodeName: "node-test", Environment: "test"},
		func() (uuid.UUID, error) { return uuid.Nil, cause },
	)
	if res != nil || !errors.Is(err, cause) {
		t.Fatalf("ошибка создания ID потеряна: %v", err)
	}
}

func TestPrepareDoesNotStartSDK(t *testing.T) {
	r, err := Prepare(ResourceSettings{NodeName: "node-test", Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if r.provider != nil || r.Reader() != nil || r.completed != nil {
		t.Fatal("Prepare создал SDK")
	}
	if err := r.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineRejectsLocalRecorder(t *testing.T) {
	r, err := New(ResourceSettings{NodeName: "node-test", Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	settings := pipelineSettings("http://localhost:4318/v1/metrics")
	p, err := StartPipeline(t.Context(), r, settings, testLogger())
	cleanupPipeline(t, p)
	if p != nil || err == nil {
		t.Fatal("экспортный provider добавлен к локальному")
	}
}

func TestPrepareInvalidSettings(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		settings ResourceSettings
	}{
		{name: "нет ноды", settings: ResourceSettings{Environment: "test"}},
		{name: "нет окружения", settings: ResourceSettings{NodeName: "node-test"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if r, err := Prepare(test.settings); r != nil || err == nil {
				t.Fatalf("приняты невалидные настройки Resource: %v", err)
			}
		})
	}
}
