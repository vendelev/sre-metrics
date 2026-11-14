package otel

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestRecorderResourceFromEnvironment(t *testing.T) {
	t.Setenv(
		"OTEL_RESOURCE_ATTRIBUTES",
		"test.source=env,service.name=attributes-service,service.namespace=env-namespace,service.instance.id=env-instance,host.name=env-node,deployment.environment.name=env-environment",
	)
	t.Setenv("OTEL_SERVICE_NAME", "env-service")
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	var collected metricdata.ResourceMetrics
	if err := r.Reader().Collect(t.Context(), &collected); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"test.source":                 "env",
		"service.name":                "env-service",
		"service.namespace":           "env-namespace",
		"service.instance.id":         "env-instance",
		"host.name":                   "env-node",
		"deployment.environment.name": "env-environment",
	} {
		got, ok := collected.Resource.Set().Value(attribute.Key(key))
		if !ok || got.AsString() != want {
			t.Fatalf("атрибут %s: получено %v, ожидалось %s", key, got, want)
		}
	}
}

func TestResourceDoesNotGenerateInstanceID(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	r, err := Prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.resource.Set().Value("service.instance.id"); ok {
		t.Fatal("service.instance.id создан без настройки окружения")
	}
}

func TestResourceAttributesFromEnvironment(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "host.name=otel-node,deployment.environment.name=test")
	r, err := Prepare()
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"host.name": "otel-node", "deployment.environment.name": "test"} {
		got, ok := r.resource.Set().Value(attribute.Key(key))
		if !ok || got.AsString() != want {
			t.Fatalf("атрибут %s из окружения: %v", key, got)
		}
	}
}

func TestPrepareDoesNotStartSDK(t *testing.T) {
	r, err := Prepare()
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
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if shutdownErr := r.Shutdown(context.Background()); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	p, err := StartPipeline(t.Context(), r, testLogger())
	cleanupPipeline(t, p)
	if p != nil || err == nil {
		t.Fatal("экспортный provider добавлен к локальному")
	}
}
