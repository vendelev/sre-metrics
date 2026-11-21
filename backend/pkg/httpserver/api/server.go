// Package api определяет контракт управления HTTP-сервером.
package api

import (
	"context"
	"net"
)

// Server предоставляет владельцу фазы запуска и остановки HTTP-сервера.
// Open открывает listener, Addr возвращает его адрес (до открытия — nil).
// Serve блокируется до завершения приёма и возвращает техническую ошибку;
// ожидаемое закрытие listener владельцем ошибкой не считается.
// CloseListener прекращает приём, Shutdown ожидает обработчики с заданным
// контекстом и при ошибке принудительно закрывает соединения, Close закрывает их
// принудительно. Владелец запускает Serve после Open и ожидает его завершения.
type Server interface {
	Open(context.Context) error
	Addr() net.Addr
	Serve() error
	CloseListener() error
	Shutdown(context.Context) error
	Close() error
}
