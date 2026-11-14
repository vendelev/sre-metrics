package bootstrap

import (
	"context"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel/metric"

	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/otel"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/queue"
	"github.com/vendelev/sre-metrics/user-login/internal/infrastructure/worker"
)

// Task публикует результат фоновой задачи модуля закрытием Done. После
// истечения бюджета владелец не ждёт произвольную Go-функцию и не читает
// ещё не готовый результат. Структура согласована с публичным Task core,
// но не связана с ним: модуль не зависит от core.
type Task struct {
	done chan struct{}
	err  error // Читается только после закрытия done.
}

// Done закрывается при завершении задачи; повторных закрытий нет.
func (t *Task) Done() <-chan struct{} {
	return t.done
}

// Err возвращает результат завершённой задачи. Вызывать только после
// закрытия Done, включая конкурентное чтение: до него значение не готово.
func (t *Task) Err() error {
	return t.err
}

// Module — публичный фасад модуля: handler и фазы запуска и остановки
// собственных фоновых задач. HTTP-ресурсы и OTel-pipeline принадлежат core;
// очередь, воркер и drain остаются во владении модуля. Поля после пустой
// строки — состояние протокола одного запуска.
type Module struct {
	recorder *otel.Recorder
	worker   *worker.Worker
	queue    *queue.EventQueue
	handler  http.Handler

	workerTask *Task
	failed     chan struct{}
}

// Handler возвращает предметный HTTP-обработчик модуля для корневой
// сборки приложения. Композицию префиксов выполняет приложение, а не модуль.
func (m *Module) Handler() http.Handler {
	return m.handler
}

// Start активирует предметные инструменты модуля в meter общего pipeline
// core. Выполняется однократно после запуска pipeline, до HTTP и обслуживания.
func (m *Module) Start(ctx context.Context, meter metric.Meter) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.recorder.Activate(meter); err != nil {
		return fmt.Errorf("активация метрик user-login: %w", err)
	}
	return ctx.Err()
}

// StartWorker запускает фоновую задачу последовательной обработки очереди.
// Ошибка задачи публикуется вместе с её результатом и сигнализируется в Failed.
func (m *Module) StartWorker(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.workerTask = &Task{done: make(chan struct{})}
	go func() {
		m.workerTask.err = m.worker.Run()
		close(m.workerTask.done)
		if m.workerTask.err != nil {
			select {
			case m.failed <- struct{}{}:
			default:
			}
		}
	}()
	return nil
}

// WorkerTask возвращает задачу воркера для ожидания завершения и чтения
// результата; до StartWorker возвращает nil.
func (m *Module) WorkerTask() *Task {
	return m.workerTask
}

// Failed сигнализирует о неожиданной ошибке фоновой задачи модуля. Буфер
// один: приложение узнаёт о первой ошибке, повторное уведомление не
// накапливается.
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
// воркером. Вызывается после HTTP Shutdown, когда новых постановок нет.
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
	case <-m.workerTask.done:
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
