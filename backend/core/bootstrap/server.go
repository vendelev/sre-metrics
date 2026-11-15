package bootstrap

import (
	"context"
	"log"
	"net"
	"net/http"

	"github.com/vendelev/sre-metrics/core/config"
	"github.com/vendelev/sre-metrics/core/internal/infrastructure/httpserver"
	"github.com/vendelev/sre-metrics/pkg/task"
)

// Server владеет HTTP-сервером и его фоновым заданием Serve одного запуска.
// Конструктор только собирает объекты: listener открывает Open на этапе
// запуска, задачу Serve — StartServe после регистрации всех маршрутов.
type Server struct {
	server *httpserver.Server
	serve  *task.Task
	failed chan struct{}
}

// NewServer собирает сервер из готового handler и журнала http.Server;
// ресурсы и фоновые задачи не открываются до этапа запуска.
func NewServer(settings config.HTTP, handler http.Handler, errorLog *log.Logger) *Server {
	return &Server{
		server: httpserver.New(settings, handler, errorLog),
		failed: make(chan struct{}, 1),
	}
}

// Open открывает listener на настроенном адресе. Ошибка bind не оставляет
// ресурсов и не запускает прием соединений.
func (s *Server) Open(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.server.Open(ctx)
}

// Addr возвращает фактический адрес listener для объявления готовности;
// до открытия возвращает nil.
func (s *Server) Addr() net.Addr {
	return s.server.Addr()
}

// StartServe запускает фоновое задание Serve. Ошибка задания публикуется
// вместе с его результатом и сигнализируется в Failed; вызов выполняется
// после регистрации всех маршрутов.
func (s *Server) StartServe(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.serve = task.New()
	go func() {
		s.serve.Complete(s.server.Serve())
		if s.serve.Err() != nil {
			select {
			case s.failed <- struct{}{}:
			default:
			}
		}
	}()
	return nil
}

// ServeTask возвращает задание Serve для ожидания завершения и чтения
// результата; до StartServe возвращает nil.
func (s *Server) ServeTask() *task.Task {
	return s.serve
}

// Failed сигнализирует об ошибке задания Serve. Буфер один: остановка
// узнаёт о первой ошибке, повторное уведомление не накапливается.
func (s *Server) Failed() <-chan struct{} {
	return s.failed
}

// CloseListener закрывает listener один раз: новые соединения не принимаются,
// уже принятые соединения продолжает обслуживать сервер до Shutdown или Close.
func (s *Server) CloseListener() error {
	return s.server.CloseListener()
}

// Shutdown дожидается завершения активных обработчиков в пределах бюджета.
// При неуспехе соединения закрываются принудительно, обе ошибки объединяются.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

// Close принудительно закрывает соединения и listener http.Server; повторный
// вызов безопасен. Он не завершает произвольный выполняющийся обработчик.
func (s *Server) Close() error {
	return s.server.Close()
}
