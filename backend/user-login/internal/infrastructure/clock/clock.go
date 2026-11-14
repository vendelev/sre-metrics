// Package clock предоставляет часы для работы с текущим временем, а не временем начала приложения.
package clock

import "time"

// RealTime возвращает текущее системное время; нулевое значение готово к использованию.
type RealTime struct{}

// Now получает время при вызове, а не при создании часов или получении HTTP-запроса.
func (RealTime) Now() time.Time {
	return time.Now()
}
