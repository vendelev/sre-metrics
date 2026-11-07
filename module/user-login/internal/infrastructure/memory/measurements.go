package memory

import (
	dEvent "github.com/vendelev/sre-metrics/user-login/internal/domain/event"
	dScenario "github.com/vendelev/sre-metrics/user-login/internal/domain/scenario"
)

func validMode(mode dEvent.OAuthMode) bool {
	return mode == dEvent.LoginModeCredentials || mode == dEvent.LoginModeExistingSession
}

func validMeasurements(measurements dScenario.Measurements) bool {
	switch measurements.Completion {
	case "",
		dScenario.OutcomeSuccess,
		dScenario.OutcomeTechnicalError,
		dScenario.OutcomeRejected,
		dScenario.OutcomeUnknown:
	default:
		return false
	}
	if measurements.TechnicalErrorMode != "" && !validMode(measurements.TechnicalErrorMode) {
		return false
	}
	if observation := measurements.SuccessfulDuration; observation != nil {
		return validMode(observation.Mode) && observation.Duration >= 0
	}
	return true
}
