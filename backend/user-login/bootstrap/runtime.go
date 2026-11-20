package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel/metric"

	"github.com/vendelev/sre-metrics/pkg/fileinbox"
	"github.com/vendelev/sre-metrics/pkg/task"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/worker"
)

// Module — публичный фасад модуля: handler и фазы запуска и остановки
// собственных фоновых задач. HTTP-ресурсы и OTel-pipeline принадлежат core;
// очередь, воркер и drain остаются во владении модуля. Поля после пустой
// строки — состояние протокола одного запуска.
type Module struct {
	recorder *otel.Recorder
	worker   *worker.Worker
	inbox    *fileinbox.Inbox
	queue    *queue.EventQueue
	handler  http.Handler

	workerTask *task.Task
	inboxTask  *task.Task
	failed     chan struct{}
}

// Handler возвращает предметный HTTP-обработчик модуля для корневой
// сборки приложения. Композицию префиксов выполняет приложение, а не модуль.
func (m *Module) Handler() http.Handler {
	return m.handler
}

// Start активирует предметные инструменты и диагностику файлового входа
// в meter общего pipeline core. Вызывается однократно до запуска входов.
func (m *Module) Start(ctx context.Context, meter metric.Meter) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.recorder.Activate(meter); err != nil {
		return fmt.Errorf("активация метрик user-login: %w", err)
	}
	if err := m.inbox.ActivateMetrics(ctx, meter); err != nil {
		return fmt.Errorf("активация метрик файлового входа: %w", err)
	}
	return ctx.Err()
}

// StartWorker запускает фоновую задачу последовательной обработки очереди.
// Ошибка задачи публикуется вместе с её результатом и сигнализируется в Failed.
func (m *Module) StartWorker(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.workerTask = task.New()
	go m.runTask(m.workerTask, m.worker.Run)
	return nil
}

// WorkerTask возвращает задачу воркера для ожидания завершения и чтения
// результата; до StartWorker возвращает nil.
func (m *Module) WorkerTask() *task.Task {
	return m.workerTask
}

// StartInbox запускает файловый вход и ждёт доступности каталога.
// При отмене запуска подаёт сигнал остановки; завершения ждёт владелец очистки.
func (m *Module) StartInbox(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	inboxTask := task.New()
	m.inboxTask = inboxTask
	go m.runTask(inboxTask, m.inbox.Run)
	select {
	case <-m.inbox.Ready():
	case <-inboxTask.Done():
		if err := inboxTask.Err(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("файловый вход завершился до готовности")
	case <-ctx.Done():
	}
	if err := ctx.Err(); err != nil {
		m.StopInbox()
		return err
	}
	return nil
}

// InboxTask возвращает задачу файлового входа или nil, если она не запущена.
func (m *Module) InboxTask() *task.Task {
	return m.inboxTask
}

// StopInbox сигнализирует об остановке без ожидания задачи и не закрывает очередь.
func (m *Module) StopInbox() {
	m.inbox.Stop()
}

func (m *Module) runTask(t *task.Task, run func() error) {
	err := run()
	t.Complete(err)
	if err != nil {
		select {
		case m.failed <- struct{}{}:
		default:
		}
	}
}

// Failed сигнализирует о неожиданной ошибке фоновой задачи модуля. Буфер
// один: уведомления могут объединяться, причины сохраняются в результатах задач.
func (m *Module) Failed() <-chan struct{} {
	return m.failed
}

// StopMaintenance прекращает планирование обслуживания и позволяет завершить
// текущую операцию. Вызывается владельцем остановки до HTTP Shutdown: воркер
// дожидается начатой операции, новые тики не планируются.
func (m *Module) StopMaintenance() {
	m.worker.StopMaintenance()
}

// CloseQueue закрывает приём: новые постановки невозможны, очередь дочитывается
// воркером. Вызывается после HTTP Shutdown и сигнала остановки файлового входа.
func (m *Module) CloseQueue() {
	m.queue.Close()
}

// Drain дожидается завершения задачи воркера в пределах переданного бюджета:
// обработка остатка очереди включена в остановку. Исчерпание бюджета отменяет
// воркер через Terminate; новые операции не планируются, незавершённый drain
// возвращает ошибку.
func (m *Module) Drain(ctx context.Context) error {
	if m.workerTask == nil {
		return nil
	}
	select {
	case <-m.workerTask.Done():
	case <-ctx.Done():
		return fmt.Errorf("исчерпан бюджет завершения обработки: %w", ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("исчерпан бюджет завершения обработки: %w", err)
	}
	return nil
}

// Terminate отменяет воркер: новые операции не планируются, запущенная операция
// дожидается завершения. Повторный вызов безопасен.
func (m *Module) Terminate() {
	m.worker.Stop()
}
