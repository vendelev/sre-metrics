package otel

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestNewRecorderUUIDError(t *testing.T) {
	cause := errors.New("синтетический сбой генерации UUID")
	r, err := newRecorder(
		ResourceSettings{NodeName: "node-test", Environment: "test"},
		func() (uuid.UUID, error) { return uuid.Nil, cause },
	)
	if r != nil || !errors.Is(err, cause) {
		t.Fatalf("ошибка создания ID потеряна: %v", err)
	}
}

func TestPreparedRecorderActivation(t *testing.T) {
	r, err := Prepare(ResourceSettings{NodeName: "node-test", Environment: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if r.provider != nil || r.Reader() != nil || r.completed != nil {
		t.Fatal("Prepare создал SDK")
	}
	res := r.resource
	if err := r.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.StartLocal(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if r.resource != res || r.provider == nil || r.Reader() == nil {
		t.Fatal("активация изменила Resource или не запустила SDK")
	}
	if err := r.StartLocal(); err == nil {
		t.Fatal("разрешён второй provider")
	}
	settings := pipelineSettings("http://localhost:4318/v1/metrics")
	if p, err := StartPipeline(t.Context(), r, settings, testLogger()); p != nil || err == nil {
		t.Fatal("экспортный provider добавлен к локальному")
	}
	if err := (*Recorder)(nil).StartLocal(); err == nil {
		t.Fatal("принят nil Recorder")
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
