package otel

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/vendelev/sre-metrics/service/user-auth/internal/domain"
)

const (
	// ScopeName — постоянное имя области инструментирования без предметных данных.
	ScopeName = "github.com/vendelev/sre-metrics/service/user-auth"
	// CompletedName — имя счётчика завершённых сценариев.
	CompletedName = "mis.login.scenarios.completed"
	// TechnicalErrorsName — имя счётчика классифицированных технических ошибок.
	TechnicalErrorsName = "mis.login.scenarios.technical_errors"
	// DurationName — имя гистограммы успешных длительностей.
	DurationName = "mis.login.scenario.duration"
)

// Recorder регистрирует измерения в локальном SDK, не выполняя сетевых операций.
// Один экземпляр принадлежит одному состоянию сервиса; глобальный provider не меняется.
type Recorder struct {
	resource        *resource.Resource
	provider        *sdkmetric.MeterProvider
	reader          *sdkmetric.ManualReader
	completed       metric.Int64Counter
	technicalErrors metric.Int64Counter
	duration        metric.Float64Histogram
}

var _ domain.MeasurementRecorder = (*Recorder)(nil)

// New создаёт инструменты, зарегистрированный reader и шесть нулевых рядов Counter.
// UUID v7 создаётся один раз. Фоновых задач и сети у локального reader нет.
func New(settings ResourceSettings) (*Recorder, error) {
	return newRecorder(settings, uuid.NewV7)
}

// Prepare создаёт один Resource и UUID, но не provider, инструменты или reader.
// До StartPipeline или StartLocal регистрировать измерения нельзя. Активация
// выполняется однократно, до запуска HTTP и обслуживания, без конкурентного доступа.
func Prepare(settings ResourceSettings) (*Recorder, error) {
	res, err := newResource(settings, uuid.NewV7)
	if err != nil {
		return nil, err
	}
	return &Recorder{resource: res}, nil
}

// StartLocal активирует подготовленный Recorder с ManualReader для изолированных
// тестов. Реальный сервис использует StartPipeline, без второго reader.
func (r *Recorder) StartLocal() error {
	if err := r.checkPrepared(); err != nil {
		return err
	}
	reader := sdkmetric.NewManualReader(sdkmetric.WithTemporalitySelector(sdkmetric.DefaultTemporalitySelector))
	active, err := recorderWithReader(r.resource, reader)
	if err != nil {
		return err
	}
	active.reader = reader
	*r = *active
	return nil
}

func (r *Recorder) checkPrepared() error {
	if r == nil || r.resource == nil || r.provider != nil {
		return fmt.Errorf("активация метрик: требуется подготовленный и ещё не запущенный Recorder")
	}
	if err := ValidateEnvironment(os.LookupEnv); err != nil {
		return err
	}
	return nil
}

func newResource(settings ResourceSettings, newID func() (uuid.UUID, error)) (*resource.Resource, error) {
	if err := ValidateEnvironment(os.LookupEnv); err != nil {
		return nil, err
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, fmt.Errorf("создание ID экземпляра метрик: %w", err)
	}
	return resource.NewSchemaless(
		attribute.String("service.namespace", "sptr"),
		attribute.String("service.name", "mis-login-observability"),
		attribute.String("service.instance.id", id.String()),
		attribute.String("host.name", settings.NodeName),
		attribute.String("deployment.environment.name", settings.Environment),
	), nil
}

func newRecorder(settings ResourceSettings, newID func() (uuid.UUID, error)) (*Recorder, error) {
	res, err := newResource(settings, newID)
	if err != nil {
		return nil, err
	}
	reader := sdkmetric.NewManualReader(sdkmetric.WithTemporalitySelector(sdkmetric.DefaultTemporalitySelector))
	r, err := recorderWithReader(res, reader)
	if r != nil {
		r.reader = reader
	}
	return r, err
}

func recorderWithReader(res *resource.Resource, reader sdkmetric.Reader) (*Recorder, error) {
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res), sdkmetric.WithReader(reader),
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOffFilter),
		// Фиксируем стандартный лимит SDK: env не должен сокращать шесть рядов baseline.
		sdkmetric.WithCardinalityLimit(2000),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: DurationName, Kind: sdkmetric.InstrumentKindHistogram},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{5, 30, 60}, NoMinMax: true,
			}},
		)),
	)
	r, err := newInstruments(provider, nil)
	if err != nil {
		// Инструменты не готовы: финальный пакет и новое сетевое ожидание не нужны.
		// В SDK v1.47.0 отменённый контекст запрещает последний сбор, но Shutdown
		// всё равно останавливает цикл и закрывает exporter. Это очистка неготовых
		// инструментов до передачи владения, а не остановка работающего pipeline.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return nil, errors.Join(err, provider.Shutdown(ctx))
	}
	r.resource = res
	for _, outcome := range []domain.Outcome{
		domain.OutcomeSuccess, domain.OutcomeTechnicalError, domain.OutcomeRejected, domain.OutcomeUnknown,
	} {
		r.completed.Add(context.Background(), 0, metric.WithAttributes(attribute.String("outcome", string(outcome))))
	}
	for _, mode := range []domain.LoginMode{domain.LoginModeCredentials, domain.LoginModeExistingSession} {
		r.technicalErrors.Add(context.Background(), 0, metric.WithAttributes(attribute.String("mode", string(mode))))
	}
	return r, nil
}

func newInstruments(provider *sdkmetric.MeterProvider, reader *sdkmetric.ManualReader) (*Recorder, error) {
	meter := provider.Meter(ScopeName)
	completed, err := meter.Int64Counter(CompletedName, metric.WithUnit("1"))
	if err != nil {
		return nil, fmt.Errorf("создание счётчика завершений: %w", err)
	}
	technicalErrors, err := meter.Int64Counter(TechnicalErrorsName, metric.WithUnit("1"))
	if err != nil {
		return nil, fmt.Errorf("создание счётчика технических ошибок: %w", err)
	}
	duration, err := meter.Float64Histogram(DurationName, metric.WithUnit("s"))
	if err != nil {
		return nil, fmt.Errorf("создание гистограммы длительностей: %w", err)
	}
	return &Recorder{provider: provider, reader: reader, completed: completed,
		technicalErrors: technicalErrors, duration: duration}, nil
}

// Record синхронно регистрирует только новые, проверенные хранилищем измерения.
// Контекст намеренно пустой: локальная регистрация не зависит от отмены запроса,
// не переносит данные запроса и не выполняет экспорт. Exemplars отключены.
func (r *Recorder) Record(measurements domain.Measurements) {
	ctx := context.Background()
	if measurements.Completion != "" {
		r.completed.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", string(measurements.Completion))))
	}
	if measurements.TechnicalErrorMode != "" {
		r.technicalErrors.Add(ctx, 1,
			metric.WithAttributes(attribute.String("mode", string(measurements.TechnicalErrorMode))))
	}
	if observation := measurements.SuccessfulDuration; observation != nil {
		r.duration.Record(ctx, observation.Duration.Seconds(),
			metric.WithAttributes(attribute.String("mode", string(observation.Mode))))
	}
}

// Reader предоставляет ManualReader локального варианта; у экспортного он nil.
func (r *Recorder) Reader() *sdkmetric.ManualReader { return r.reader }

// Shutdown освобождает SDK после прекращения обработки. В экспортном варианте
// вызывающий использует Pipeline.Shutdown, который также освобождает transport.
func (r *Recorder) Shutdown(ctx context.Context) error {
	if r.provider == nil {
		return nil
	}
	return r.provider.Shutdown(ctx)
}
