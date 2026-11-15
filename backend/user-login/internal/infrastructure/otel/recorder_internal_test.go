package otel

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
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

func TestPrepareDoesNotStartSDK(t *testing.T) {
	r := Prepare()
	if r.provider != nil || r.Reader() != nil || r.completed != nil {
		t.Fatal("Prepare создал SDK или инструменты")
	}
	if err := r.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// Активация выполняется однократно; изолированный тестовый вариант уже содержит
// инструменты, рабочий — требует meter запущенного pipeline.
func TestActivateRejectsRepeatedActivation(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if shutdownErr := r.Shutdown(context.Background()); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	if err := r.Activate(nil); err == nil {
		t.Fatal("принят nil meter")
	}
	provider := sdkmetric.NewMeterProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	if err := r.Activate(provider.Meter("duplicate")); err == nil {
		t.Fatal("разрешена повторная активация")
	}
}
