package httpserver

import (
	"net"
	"sync"
	"sync/atomic"
)

// listener закрывается один раз, в том числе при частично успешном запуске.
// Признак closing отличает штатное закрытие владельцем от отказа Accept
// при интерпретации результата Serve.
type listener struct {
	net.Listener
	once    sync.Once
	err     error // Читается после once.Do, включая конкурентное повторное закрытие.
	closing atomic.Bool
}

func (l *listener) Close() error {
	l.once.Do(func() {
		l.closing.Store(true)
		l.err = l.Listener.Close()
	})
	return l.err
}
