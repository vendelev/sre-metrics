package contract

import "time"

// Clock предоставляет текущее время Go при приёме и обработке событий.
type Clock interface {
	// Now возвращает текущее время Go для динамических проверок операции.
	Now() time.Time
}
