package memory

import "github.com/vendelev/sre-metrics/service/user-auth/internal/domain"

func validMode(mode domain.LoginMode) bool {
	return mode == domain.LoginModeCredentials || mode == domain.LoginModeExistingSession
}

func validMeasurements(measurements domain.Measurements) bool {
	switch measurements.Completion {
	case "", domain.OutcomeSuccess, domain.OutcomeTechnicalError, domain.OutcomeRejected, domain.OutcomeUnknown:
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
