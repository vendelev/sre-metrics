package otel

import (
	"context"
	"errors"
	"fmt"

	dContract "github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	dScenario "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/resource"
)

const (
	// ScopeName — постоянное имя области инструментирования без предметных данных.
	ScopeName = "sre-metrics/user-login"
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

var _ dContract.MeasurementRecorder = (*Recorder)(nil)

// New создаёт инструменты, зарегистрированный reader и шесть нулевых рядов Counter.
// Resource читается из окружения. Фоновых задач и сети у локального reader нет.
func New() (*Recorder, error) {
	res := resource.Environment()
	reader := sdkmetric.NewManualReader(sdkmetric.WithTemporalitySelector(sdkmetric.DefaultTemporalitySelector))
	r, err := recorderWithReader(res, reader)
	if r != nil {
		r.reader = reader
	}
	return r, err
}

// Prepare фиксирует Resource из окружения, но не создаёт provider, инструменты или reader.
// До StartPipeline регистрировать измерения нельзя. Активация
// выполняется однократно, до запуска HTTP и обслуживания, без конкурентного доступа.
func Prepare() (*Recorder, error) {
	return &Recorder{resource: resource.Environment()}, nil
}

func (r *Recorder) checkPrepared() error {
	if r == nil || r.resource == nil || r.provider != nil {
		return fmt.Errorf("активация метрик: требуется подготовленный и ещё не запущенный Recorder")
	}
	return nil
}

func recorderWithReader(res *resource.Resource, reader sdkmetric.Reader) (*Recorder, error) {
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOffFilter),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: DurationName, Kind: sdkmetric.InstrumentKindHistogram},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
					Boundaries: []float64{5, 30, 60}, NoMinMax: true,
				},
			},
		)),
	)
	r, err := newInstruments(provider)
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
	for _, outcome := range []dScenario.Outcome{
		dScenario.OutcomeSuccess,
		dScenario.OutcomeTechnicalError,
		dScenario.OutcomeRejected,
		dScenario.OutcomeUnknown,
	} {
		r.completed.Add(context.Background(), 0, metric.WithAttributes(attribute.String("outcome", string(outcome))))
	}
	for _, mode := range []dEvent.OAuthMode{
		dEvent.LoginModeCredentials,
		dEvent.LoginModeExistingSession,
	} {
		r.technicalErrors.Add(context.Background(), 0, metric.WithAttributes(attribute.String("mode", string(mode))))
	}
	return r, nil
}

func newInstruments(provider *sdkmetric.MeterProvider) (*Recorder, error) {
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
	return &Recorder{provider: provider, completed: completed,
		technicalErrors: technicalErrors, duration: duration}, nil
}

// Record синхронно регистрирует новые измерения, сформированные domain через TakeMeasurements.
// Контекст намеренно пустой: локальная регистрация не зависит от отмены запроса,
// не переносит данные запроса и не выполняет экспорт. Exemplars отключены.
func (r *Recorder) Record(measurements dScenario.Measurements) {
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
// вызывающий использует Pipeline.Shutdown.
func (r *Recorder) Shutdown(ctx context.Context) error {
	if r.provider == nil {
		return nil
	}
	return r.provider.Shutdown(ctx)
}
