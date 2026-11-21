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

// Recorder регистрирует измерения входа, не выполняя сетевых операций.
// Рабочий вариант не создаёт provider: инструменты регистрирует Activate
// в meter общего pipeline, которому принадлежит provider; освобождает его
// Pipeline.Shutdown. Изолированный тестовый вариант создаёт provider
// с ManualReader через New. Один экземпляр принадлежит одному состоянию
// сервиса; глобальный provider не меняется.
type Recorder struct {
	provider        *sdkmetric.MeterProvider // Только изолированный тестовый вариант.
	reader          *sdkmetric.ManualReader
	completed       metric.Int64Counter
	technicalErrors metric.Int64Counter
	duration        metric.Float64Histogram
}

var _ dContract.MeasurementRecorder = (*Recorder)(nil)

// New создаёт изолированный SDK: инструменты, View гистограммы,
// зарегистрированный ManualReader и шесть нулевых рядов Counter. Resource
// читается из окружения. Фоновых задач и сети у локального reader нет;
// вариант предназначен для тестов.
func New() (*Recorder, error) {
	res := resource.Environment()
	reader := sdkmetric.NewManualReader(sdkmetric.WithTemporalitySelector(sdkmetric.DefaultTemporalitySelector))
	r, err := recorderWithReader(res, reader)
	if r != nil {
		r.reader = reader
	}
	return r, err
}

// Prepare возвращает Recorder рабочего варианта без provider и инструментов.
// Инструменты регистрирует Activate в meter запущенного pipeline; до неё
// регистрировать измерения нельзя. Активация выполняется однократно, до запуска
// HTTP и обслуживания, без конкурентного доступа.
func Prepare() *Recorder { return &Recorder{} }

// Activate регистрирует инструменты и нулевые ряды Counter в meter запущенного
// pipeline. Границы гистограммы задаёт View модуля (HistogramView),
// переданный в pipeline при его сборке. Активация выполняется однократно;
// освобождение provider выполняет Shutdown pipeline, а не Recorder.
func (r *Recorder) Activate(meter metric.Meter) error {
	if meter == nil {
		return fmt.Errorf("активация метрик: требуется meter запущенного pipeline")
	}
	if r.completed != nil {
		return fmt.Errorf("активация метрик: требуется ещё не активированный Recorder")
	}
	partial, err := newInstruments(meter)
	if err != nil {
		return err
	}
	r.completed = partial.completed
	r.technicalErrors = partial.technicalErrors
	r.duration = partial.duration
	r.addZeroRows()
	return nil
}

// HistogramView задаёт явные границы и отключение min/max для гистограммы
// длительностей. View применяется при создании provider, поэтому передаётся
// в общий pipeline при его сборке.
func HistogramView() sdkmetric.View {
	return sdkmetric.NewView(
		sdkmetric.Instrument{Name: DurationName, Kind: sdkmetric.InstrumentKindHistogram},
		sdkmetric.Stream{
			Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{5, 30, 60}, NoMinMax: true,
			},
		},
	)
}

// recorderWithReader создаёт provider изолированного тестового варианта
// с инструментами и нулевыми рядами.
func recorderWithReader(res *resource.Resource, reader sdkmetric.Reader) (*Recorder, error) {
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
		// Общий pipeline отключает exemplars той же настройкой.
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOffFilter),
		sdkmetric.WithView(HistogramView()),
	)
	r, err := newInstruments(provider.Meter(ScopeName))
	if err != nil {
		// Инструменты не готовы: финальный пакет и новое сетевое ожидание не нужны.
		// В SDK v1.47.0 отменённый контекст запрещает последний сбор, но Shutdown
		// всё равно останавливает цикл и освобождает reader. Это очистка неготовых
		// инструментов до передачи владения, а не остановка работающего pipeline.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return nil, errors.Join(err, provider.Shutdown(ctx))
	}
	r.provider = provider
	r.addZeroRows()
	return r, nil
}

// newInstruments создаёт три инструмента входа в переданном meter.
func newInstruments(meter metric.Meter) (*Recorder, error) {
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
	return &Recorder{completed: completed, technicalErrors: technicalErrors, duration: duration}, nil
}

// addZeroRows создаёт шесть нулевых рядов Counter до HTTP и обслуживания:
// все исходы и режимы присутствуют в первом экспорте, даже если измерений ещё не было.
func (r *Recorder) addZeroRows() {
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

// Activated сообщает, зарегистрированы ли инструменты: до Activate регистрация
// измерений невозможна. Рабочая сборка активирует Recorder в meter общего
// pipeline; изолированный тестовый вариант создаёт инструменты сразу.
func (r *Recorder) Activated() bool { return r.completed != nil }

// Reader предоставляет ManualReader локального варианта; у экспортного он nil.
func (r *Recorder) Reader() *sdkmetric.ManualReader { return r.reader }

// Shutdown освобождает provider изолированного тестового варианта. Рабочий
// вариант provider не создаёт: освобождение выполняет Shutdown pipeline.
func (r *Recorder) Shutdown(ctx context.Context) error {
	if r.provider == nil {
		return nil
	}
	return r.provider.Shutdown(ctx)
}
