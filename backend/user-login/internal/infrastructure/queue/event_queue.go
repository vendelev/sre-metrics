// Package queue реализует ограниченную очередь предметных событий в памяти.
// Постановка не ожидает свободного места; закрытие синхронизировано с отправителями.
package queue

import (
	"errors"
	"sync"

	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
)

// ErrClosed означает, что приём остановлен; это не переполнение очереди.
var ErrClosed = errors.New("приём событий остановлен")

// EventQueue отделяет проверку приёма от обработки. Блокировка защищает только
// постановку и закрытие канала, но не состояние сценариев или работу воркера.
type EventQueue struct {
	queue  chan *dEvent.Request
	mu     sync.RWMutex
	closed bool
}

// NewEventQueue создаёт очередь с уже проверенным конфигурацией положительным размером.
func NewEventQueue(size int) *EventQueue {
	return &EventQueue{queue: make(chan *dEvent.Request, size)}
}

// Events возвращает канал для чтения воркером; постановка и закрытие остаются методами очереди.
func (q *EventQueue) Events() <-chan *dEvent.Request { return q.queue }

// Enqueue отправляет событие из HTTP в очередь.
func (q *EventQueue) Enqueue(event *dEvent.Request) error {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return ErrClosed
	}
	select {
	case q.queue <- event:
		return nil
	default:
		return dError.ErrOverloaded
	}
}

// Close запрещает новые постановки и оставляет принятые события для drain.
// Повторное закрытие безопасно, в том числе при отказе воркера и остановке сервиса.
func (q *EventQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		close(q.queue)
	}
}
