// Package login will coordinate event processing and scenario expiration.
// It owns the required storage and measurement ports; concrete adapters implement
// those ports. Atomic processing semantics must follow the login specification.
package login
