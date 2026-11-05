// Package clock предоставляет реальные часы для операций приложения.
package clock

import "time"

// Real возвращает текущее системное время; нулевое значение готово к использованию.
type Real struct{}

// Now получает время при вызове, а не при создании часов или получении HTTP-запроса.
func (Real) Now() time.Time { return time.Now() }
