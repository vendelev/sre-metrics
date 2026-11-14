package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

// Config содержит параметры net/http Server. Заполняется сборкой из уже
// проверенной конфигурации сервиса; формат адреса проверяет ядро при Open.
type Config struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

// Server владеет HTTP-ресурсами одного запуска. Конструктор только собирает
// объекты: listener открывает Open на этапе запуска и закрывает один раз,
// в том числе при частично успешном запуске.
type Server struct {
	server   *http.Server
	listener *listener
}

// New собирает сервер из готового handler и журнала http.Server; ресурсы
// не открываются до Open.
func New(config Config, handler http.Handler, errorLog *log.Logger) *Server {
	return &Server{
		server: &http.Server{
			Addr:              config.Addr,
			Handler:           handler,
			ReadHeaderTimeout: config.ReadHeaderTimeout,
			ReadTimeout:       config.ReadTimeout,
			WriteTimeout:      config.WriteTimeout,
			IdleTimeout:       config.IdleTimeout,
			ErrorLog:          errorLog,
		},
	}
}

// Open открывает listener на настроенном адресе. Ошибка bind не оставляет
// ресурсов и не запускает прием соединений.
func (s *Server) Open(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var listenConfig net.ListenConfig
	ln, err := listenConfig.Listen(ctx, "tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("запуск HTTP user-login на %s: %w", s.server.Addr, err)
	}
	s.listener = &listener{Listener: ln}
	return ctx.Err()
}

// Addr возвращает фактический адрес listener для объявления готовности;
// до открытия возвращает nil.
func (s *Server) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Serve принимает соединения до закрытия listener и возвращает техническую
// ошибку. Закрытие при остановке не считается ошибкой: если listener закрыт
// владельцем, ErrServerClosed и net.ErrClosed от Serve скрываются.
func (s *Server) Serve() error {
	err := s.server.Serve(s.listener)
	if s.listener.closing.Load() && (errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed)) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("работа HTTP user-login: %w", err)
	}
	return nil
}

// Shutdown дожидается завершения активных обработчиков в пределах бюджета.
// При неуспехе соединения закрываются принудительно, обе ошибки объединяются.
func (s *Server) Shutdown(ctx context.Context) error {
	err := shutdownError(s.server.Shutdown(ctx))
	if err != nil {
		err = errors.Join(err, s.Close())
	}
	return err
}

// Close принудительно закрывает соединения и listener http.Server; повторный
// вызов безопасен. Он не завершает произвольный выполняющийся обработчик.
func (s *Server) Close() error {
	return shutdownError(s.server.Close())
}

// CloseListener закрывает listener один раз: новые соединения не принимаются,
// уже принятые соединения продолжает обслуживать http.Server до Shutdown
// или Close. До открытия listener повторный вызов безопасен.
func (s *Server) CloseListener() error {
	if s.listener == nil {
		return nil
	}
	if err := s.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("закрытие listener user-login: %w", err)
	}
	return nil
}

func shutdownError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("остановка HTTP user-login: %w", err)
}
