// Package userauth assembles and runs the authentication metrics service.
package userauth

import (
	"context"
	"errors"
)

// Run is the composition root. The scaffold intentionally starts no listeners
// or workers; implementation will wire configuration, use cases, and adapters.
func Run(_ context.Context) error {
	return errors.New("user-auth: scaffold only; event processing and telemetry are not implemented")
}
