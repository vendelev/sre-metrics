package config

import (
	"fmt"
	"time"
)

// Processing задаёт лимиты очереди и сценариев, а также интервал их обслуживания.
type Processing struct {
	EventQueueSize       int           `env:"USER_LOGIN_EVENT_QUEUE_SIZE"`
	MaxScenarios         int           `env:"USER_LOGIN_MAX_SCENARIOS"`
	MaxEventsPerScenario int           `env:"USER_LOGIN_MAX_EVENTS_PER_SCENARIO"`
	MaintenanceInterval  time.Duration `env:"USER_LOGIN_MAINTENANCE_INTERVAL"`
}

func defaultProcessing() Processing {
	return Processing{
		EventQueueSize:       1024,
		MaxScenarios:         10_000,
		MaxEventsPerScenario: 32,
		MaintenanceInterval:  time.Second,
	}
}

func (p Processing) validatePositive() error {
	return validatePositive(
		numericSetting{name: "EventQueueSize", value: int64(p.EventQueueSize)},
		numericSetting{name: "MaxScenarios", value: int64(p.MaxScenarios)},
		numericSetting{name: "MaxEventsPerScenario", value: int64(p.MaxEventsPerScenario)},
		numericSetting{name: "MaintenanceInterval", value: int64(p.MaintenanceInterval)},
	)
}

func (p Processing) validateMaintenanceInterval() error {
	if p.MaintenanceInterval > time.Minute {
		return fmt.Errorf("%s: интервал не должен превышать 1m", envName("MaintenanceInterval"))
	}
	return nil
}
