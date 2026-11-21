// Package watcher адаптирует файловые уведомления к планированию сканирования.
package watcher

import "github.com/fsnotify/fsnotify"

// Watcher предоставляет каналы уведомлений наблюдаемого каталога.
type Watcher interface {
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	Close() error
}

type notify struct {
	watcher *fsnotify.Watcher
}

// New начинает наблюдение каталога; владелец закрывает его вместе с циклом приёма.
func New(dir string) (Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := watcher.Add(dir); err != nil {
		// Возвращаем исходную причину: наблюдение не было установлено.
		_ = watcher.Close()
		return nil, err
	}
	return &notify{watcher: watcher}, nil
}

// Events возвращает уведомления об изменении записей каталога.
func (n *notify) Events() <-chan fsnotify.Event {
	return n.watcher.Events
}

// Errors возвращает ошибки наблюдения, включая переполнение уведомлений.
func (n *notify) Errors() <-chan error {
	return n.watcher.Errors
}

// Close освобождает наблюдение каталога.
func (n *notify) Close() error {
	return n.watcher.Close()
}
