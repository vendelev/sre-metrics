package error_test

import (
	"errors"
	"testing"
	"time"

	dError "github.com/vendelev/sre-metrics/user-login/internal/domain/error"
)

// BR-37, AC-26: обе границы допустимого возраста включительны.
func TestCheckEventTime(t *testing.T) {
	now := time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		offset    time.Duration
		wantError error
	}{
		{name: "старше нижней границы", offset: -30*time.Minute - time.Millisecond, wantError: dError.ErrEventTime},
		{name: "нижняя граница", offset: -30 * time.Minute},
		{name: "текущее время", offset: 0},
		{name: "верхняя граница", offset: 30 * time.Second},
		{name: "дальше верхней границы", offset: 30*time.Second + time.Millisecond, wantError: dError.ErrEventTime},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := dError.CheckEventTime(now.Add(tt.offset), now); !errors.Is(err, tt.wantError) {
				t.Errorf("возраст: %v, ожидалось %v", err, tt.wantError)
			}
		})
	}
}
