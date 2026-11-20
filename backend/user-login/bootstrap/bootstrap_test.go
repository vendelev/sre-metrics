package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/vendelev/sre-metrics/pkg/clock"
	"github.com/vendelev/sre-metrics/pkg/task"
	userloginconfig "github.com/vendelev/sre-metrics/user-login/config"
	"github.com/vendelev/sre-metrics/user-login/internal/application/usecase"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/contract"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/memory"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/worker"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// barrierClock приостанавливает одну предметную операцию на существующей
// границе часов. Это позволяет воспроизвести незавершённое обслуживание без
// хука в рабочем коде.
type barrierClock struct {
	*testClock
	armed                    atomic.Bool
	entered, release, exited chan struct{}
}

func newBarrierClock(now time.Time) *barrierClock {
	return &barrierClock{
		testClock: &testClock{now: now},
		entered:   make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{}),
	}
}

func (c *barrierClock) Now() time.Time {
	if c.armed.CompareAndSwap(true, false) {
		close(c.entered)
		<-c.release
		defer close(c.exited)
	}
	return c.testClock.Now()
}

// Реальный memory не выдаёт произвольную техническую ошибку. Подмена
// существующего domain.Store нужна только для проверки публикации причины
// отказа фоновым задачам модуля.
type failedStore struct {
	contract.Store
	err    error
	cancel context.CancelFunc
}

func (s failedStore) RemoveRetained(time.Time) error {
	if s.cancel != nil {
		s.cancel()
	}
	return s.err
}

// ingestEvent воспроизводит событие приёма для проверок очереди модуля.
func ingestEvent(id int, kind dEvent.Type, at time.Time) *dEvent.Request {
	return &dEvent.Request{
		EventID:    uuid.MustParse(fmt.Sprintf("019aabcd-0000-7000-8000-%012x", id)),
		ScenarioID: uuid.MustParse("019aabcd-0000-7000-9000-000000000100"),
		OccurredAt: at,
		Type:       kind,
		Source:     dEvent.SourceMIS,
	}
}

// waitSignal ограничивает ожидание сигнала тайм-аутом, чтобы сбой теста не
// подвешивал набор.
func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("не дождались сигнала")
	}
}

func waitResult(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("не дождались завершения задачи")
	}
}

// mustModule собирает модуль с настоящим изолированным Recorder (тестовый
// вариант с ManualReader) и подменённым хранилищем для проверок публикации
// ошибок и drain фоновых задач.
func mustModule(
	t *testing.T,
	store contract.Store,
	domainClock clock.Clock,
	interval time.Duration,
) *Module {
	t.Helper()
	recorder, err := otel.New()
	if err != nil {
		t.Fatal(err)
	}
	if store == nil {
		limits := memory.DefaultLimits()
		store, err = memory.NewStore(limits, recorder)
		if err != nil {
			t.Fatal(err)
		}
	}
	if domainClock == nil {
		domainClock = clock.RealTime{}
	}
	observer, err := usecase.NewLoginObserver(store, domainClock)
	if err != nil {
		t.Fatal(err)
	}
	events := queue.NewEventQueue(2)
	w, err := worker.New(observer, events.Events(), events.Close, interval, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return mustModuleWithInbox(t, recorder, w, events)
}

func mustModuleWithInbox(t *testing.T, recorder *otel.Recorder, w *worker.Worker, events *queue.EventQueue) *Module {
	t.Helper()
	cfg := userloginconfig.Default().Inbox
	cfg.Dir = t.TempDir()
	logger := slog.New(slog.DiscardHandler)
	input, err := inboxOf(cfg, events, logger, clock.RealTime{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewModule(recorder, w, input, events, logger, clock.RealTime{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Рабочая сборка не активирует инструменты и не создаёт provider: активацию
// выполняет Start в meter запущенного pipeline core.
func TestModuleBuildDoesNotActivateInstruments(t *testing.T) {
	t.Parallel()
	recorder := otel.Prepare()
	limits := memory.DefaultLimits()
	store, err := memory.NewStore(limits, recorder)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := usecase.NewLoginObserver(store, clock.RealTime{})
	if err != nil {
		t.Fatal(err)
	}
	events := queue.NewEventQueue(2)
	w, err := worker.New(observer, events.Events(), events.Close, time.Second, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	m := mustModuleWithInbox(t, recorder, w, events)
	if m.recorder.Activated() {
		t.Fatal("инструменты активированы при сборке")
	}
	if m.recorder.Reader() != nil {
		t.Fatal("ManualReader в рабочей сборке модуля")
	}
	if m.WorkerTask() != nil || m.InboxTask() != nil {
		t.Fatal("фоновая задача запущена при сборке")
	}
	if err := m.Start(t.Context(), nil); err == nil {
		t.Fatal("активация без meter должна требовать запущенный pipeline")
	}
}

// Неожиданная ошибка обработки публикуется в результате задачи воркера,
// сигнализируется приложению и закрывает приём: новые постановки невозможны.
func TestWorkerFailurePublishesTaskError(t *testing.T) {
	cause := errors.New("синтетический отказ обработки очереди")
	m := mustModule(t, failedStore{err: cause}, nil, time.Second)
	if err := m.queue.Enqueue(ingestEvent(1, dEvent.LoginSucceeded, time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := m.StartWorker(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitResult(t, m.Failed())
	if !errors.Is(m.WorkerTask().Err(), cause) {
		t.Fatalf("сбой очереди не опубликован: %v", m.WorkerTask().Err())
	}
	if err := m.queue.Enqueue(ingestEvent(2, dEvent.LoginSucceeded, time.Now())); err == nil {
		t.Fatal("при отказе воркера приём остался открыт")
	}
	m.Terminate()
}

// Ошибка обслуживания не маскируется и сохраняется в задаче воркера даже при
// одновременной отмене владельцем запуска.
func TestMaintenanceFailurePublishesTaskError(t *testing.T) {
	for _, withCancellation := range []bool{false, true} {
		name := "без отмены"
		if withCancellation {
			name = "одновременно с отменой"
		}
		t.Run(name, func(t *testing.T) {
			cause := errors.New("управляемый сбой обслуживания")
			store := failedStore{err: cause}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if withCancellation {
				store.cancel = cancel
			}
			m := mustModule(t, store, nil, time.Millisecond)
			if err := m.StartWorker(ctx); err != nil {
				t.Fatal(err)
			}
			waitResult(t, m.WorkerTask().Done())
			if !errors.Is(m.WorkerTask().Err(), cause) {
				t.Fatalf("сбой обслуживания замаскирован: %v", m.WorkerTask().Err())
			}
			m.Terminate()
		})
	}
}

// Для точной проверки бюджета достаточно фазы ожидания без сети: Drain ждёт
// задачу воркера ровно переданный бюджет и не выдаёт новый при повторе.
func TestDrainUsesGivenBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := mustModule(t, nil, nil, time.Second)
		m.workerTask = task.New()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		started := time.Now()
		err := m.Drain(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("исчерпание бюджета drain потеряно: %v", err)
		}
		if elapsed := time.Since(started); elapsed != time.Second {
			t.Fatalf("drain получил отдельный бюджет: %s", elapsed)
		}
		before := time.Now()
		if err := m.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Now() != before {
			t.Fatalf("повторный drain получил новый бюджет: %v", err)
		}
		m.workerTask.Complete(nil)
	})
}

// Исчерпание бюджета отменяет воркер: запущенная операция дообрабатывается,
// следующее событие не запускается, а отмена владельцем не является ошибкой
// задачи. Повторные подфазы остановки не создают новый бюджет.
func TestModuleDrainDeadlineLeavesRestQueue(t *testing.T) {
	c := newBarrierClock(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	m := mustModule(t, nil, c, time.Second)
	if err := m.queue.Enqueue(ingestEvent(1, dEvent.LoginSucceeded, c.Now())); err != nil {
		t.Fatal(err)
	}
	if err := m.queue.Enqueue(ingestEvent(2, dEvent.LoginSucceeded, c.Now())); err != nil {
		t.Fatal(err)
	}
	c.armed.Store(true)
	if err := m.StartWorker(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, c.entered)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := m.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("незавершённая очередь скрыта: %v", err)
	}
	m.CloseQueue()
	if err := m.queue.Enqueue(ingestEvent(3, dEvent.LoginSucceeded, c.Now())); err == nil {
		t.Fatal("после остановки приём открыт")
	}
	close(c.release)
	m.Terminate()
	waitResult(t, m.WorkerTask().Done())
	if m.WorkerTask().Err() != nil {
		t.Fatalf("отмена по бюджету добавила ошибку воркера: %v", m.WorkerTask().Err())
	}
	if len(m.queue.Events()) != 1 {
		t.Fatal("после исчерпания бюджета запущено следующее событие")
	}
}

// Завершённая задача не блокирует drain: ожидание завершается без ошибки.
func TestDrainReturnsAfterWorkerTask(t *testing.T) {
	m := mustModule(t, nil, nil, time.Second)
	m.workerTask = task.New()
	m.workerTask.Complete(nil)
	if err := m.Drain(t.Context()); err != nil {
		t.Fatalf("drain завершённой задачи вернул ошибку: %v", err)
	}
}

// Подфазы остановки безопасны до запуска задачи воркера: очистка частичного
// запуска не требует знания состояния фаз.
func TestModuleStopPhasesWithoutWorkerTask(t *testing.T) {
	t.Parallel()
	m := mustModule(t, nil, nil, time.Second)
	if err := m.Drain(t.Context()); err != nil {
		t.Fatalf("drain без задачи воркера вернул ошибку: %v", err)
	}
	m.CloseQueue()
	m.StopMaintenance()
	m.Terminate()
}
