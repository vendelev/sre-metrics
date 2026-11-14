package event_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vendelev/sre-metrics/user-login/internal/domain/event"
)

func TestRequestClone(t *testing.T) {
	for _, withMode := range []bool{false, true} {
		name := "без варианта"
		if withMode {
			name = "с вариантом"
		}
		t.Run(name, func(t *testing.T) {
			original := event.Request{
				EventID: uuid.UUID{1}, ScenarioID: uuid.UUID{2},
				OccurredAt: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC),
				Type:       event.LoginStarted, Source: event.SourceMIS,
			}
			if withMode {
				mode := event.LoginModeCredentials
				original.Type, original.Source, original.AuthMode = event.LoginModeDetermined, event.SourceAuth, &mode
			}
			copy := original.Clone()
			if !reflect.DeepEqual(original, copy) {
				t.Fatal("копия отличается от исходного события")
			}
			copy.EventID = uuid.UUID{3}
			if original.EventID != (uuid.UUID{1}) {
				t.Fatal("изменение копии изменило исходный ID")
			}
			if withMode {
				*copy.AuthMode = event.LoginModeExistingSession
				if *original.AuthMode != event.LoginModeCredentials {
					t.Fatal("копия сохранила общий указатель на вариант")
				}
			}
		})
	}
}

// BR-37, BR-53: ParseTime проверяет wire-формат, календарную дату и возраст.
func TestParseTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		value     string
		want      time.Time
		wantError bool
	}{
		{name: "миллисекунды с точкой", value: "2026-11-02T12:00:00.123Z", want: now.Add(123 * time.Millisecond)},
		{name: "миллисекунды с запятой", value: "2026-11-02T12:00:00,123Z", want: now.Add(123 * time.Millisecond)},
		{name: "нижняя граница", value: "2026-11-02T11:30:00.000Z", want: now.Add(-30 * time.Minute)},
		{name: "после нижней границы", value: "2026-11-02T11:30:00.001Z", want: now.Add(-30*time.Minute + time.Millisecond)},
		{name: "текущее время", value: "2026-11-02T12:00:00.000Z", want: now},
		{name: "перед верхней границей", value: "2026-11-02T12:00:29.999Z", want: now.Add(30*time.Second - time.Millisecond)},
		{name: "верхняя граница", value: "2026-11-02T12:00:30.000Z", want: now.Add(30 * time.Second)},
		{name: "старше нижней границы", value: "2026-11-02T11:29:59.999Z", wantError: true},
		{name: "дальше верхней границы", value: "2026-11-02T12:00:30.001Z", wantError: true},
		{name: "пустая строка", wantError: true},
		{name: "не время", value: "не время", wantError: true},
		{name: "некорректная дата", value: "2026-11-31T12:00:00.000Z", wantError: true},
		{name: "некорректные часы", value: "2026-11-02T24:00:00.000Z", wantError: true},
		{name: "нет миллисекунд", value: "2026-11-02T12:00:00Z", wantError: true},
		{name: "мало знаков миллисекунд", value: "2026-11-02T12:00:00.12Z", wantError: true},
		{name: "много знаков миллисекунд", value: "2026-11-02T12:00:00.1234Z", wantError: true},
		{name: "нет Z", value: "2026-11-02T12:00:00.000", wantError: true},
		{name: "смещение вместо Z", value: "2026-11-02T12:00:00.000+00:00", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := event.ParseTime(tc.value, now)
			if (err != nil) != tc.wantError {
				t.Fatalf("разбор %q: %v, ожидалась ошибка: %t", tc.value, err, tc.wantError)
			}
			if err == nil && !got.Equal(tc.want) {
				t.Fatalf("разбор %q: получено %s, ожидалось %s", tc.value, got, tc.want)
			}
		})
	}
}
