package application

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vendelev/sre-metrics/user-login/internal/domain"
)

func validEvent() domain.Event {
	return domain.Event{
		EventID: "019aabcd-0000-7000-8000-000000000001", ScenarioID: "019aabcd-0000-7000-8000-000000000100",
		OccurredAt: time.Date(2026, time.November, 2, 12, 0, 0, 0, time.UTC),
		Type:       domain.EventTypeLoginStarted, Source: domain.SourceMIS,
	}
}

func TestPrepareEventInvalidDirectCalls(t *testing.T) {
	cases := []struct {
		name   string
		change func(*domain.Event)
	}{
		{name: "нет идентификатора события", change: func(e *domain.Event) { e.EventID = "" }},
		{name: "нет идентификатора сценария", change: func(e *domain.Event) { e.ScenarioID = "" }},
		{name: "не hex", change: func(e *domain.Event) { e.EventID = "019aabcd-0000-7000-8000-00000000000g" }},
		{name: "не v7", change: func(e *domain.Event) { e.EventID = "019aabcd-0000-4000-8000-000000000001" }},
		{name: "не variant 10", change: func(e *domain.Event) { e.EventID = "019aabcd-0000-7000-7000-000000000001" }},
		{name: "нет дефиса", change: func(e *domain.Event) { e.EventID = "019aabcdx0000-7000-8000-000000000001" }},
		{
			name:   "uuid без дефисов",
			change: func(e *domain.Event) { e.EventID = strings.ReplaceAll(e.EventID, "-", "") },
		},
		{name: "uuid в скобках", change: func(e *domain.Event) { e.EventID = "{" + e.EventID + "}" }},
		{name: "uuid в формате urn", change: func(e *domain.Event) { e.EventID = "urn:uuid:" + e.EventID }},
		{name: "неизвестный тип", change: func(e *domain.Event) { e.Type = "login_unknown" }},
		{name: "неизвестный источник", change: func(e *domain.Event) { e.Source = "browser" }},
		{name: "начало не от мис", change: func(e *domain.Event) { e.Source = domain.SourceAuth }},
		{name: "вариант без mode", change: func(e *domain.Event) {
			e.Type = domain.EventTypeLoginModeDetermined
			e.Source = domain.SourceAuth
		}},
		{name: "вариант не от auth", change: func(e *domain.Event) {
			e.Type = domain.EventTypeLoginModeDetermined
			mode := domain.LoginModeCredentials
			e.Mode = &mode
		}},
		{name: "неизвестный mode", change: func(e *domain.Event) {
			e.Type = domain.EventTypeLoginModeDetermined
			e.Source = domain.SourceAuth
			mode := domain.LoginMode("unknown")
			e.Mode = &mode
		}},
		{name: "лишний mode", change: func(e *domain.Event) {
			mode := domain.LoginModeCredentials
			e.Mode = &mode
		}},
		{name: "точность меньше миллисекунды", change: func(e *domain.Event) {
			e.OccurredAt = e.OccurredAt.Add(time.Nanosecond)
		}},
		{name: "год вне rfc3339", change: func(e *domain.Event) { e.OccurredAt = e.OccurredAt.AddDate(10_000, 0, 0) }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			e := validEvent()
			tt.change(&e)
			// Хранилища и часов нет: структурный отказ должен произойти раньше обращения к ним.
			observer := &LoginObserver{}
			if _, err := observer.Process(e); !errors.Is(err, domain.ErrInvalidEvent) {
				t.Fatalf("прямой вызов: %v", err)
			}
		})
	}
}

func TestPrepareEventNormalization(t *testing.T) {
	e := validEvent()
	e.EventID = strings.ToUpper(e.EventID)
	e.ScenarioID = strings.ToUpper(e.ScenarioID)
	e.Type = domain.EventTypeLoginModeDetermined
	e.Source = domain.SourceAuth
	mode := domain.LoginModeExistingSession
	e.Mode = &mode
	prepared, err := prepareEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.EventID != strings.ToLower(e.EventID) || prepared.ScenarioID != strings.ToLower(e.ScenarioID) {
		t.Fatal("идентификаторы не нормализованы одинаково")
	}
	mode = domain.LoginModeCredentials
	if *prepared.Mode != domain.LoginModeExistingSession {
		t.Fatal("входной указатель не скопирован")
	}
}
