package peer

import "strings"

const redactedMarker = "[REDACTED]"

// redactToken scrubs the session token from a free-form string. It exists
// because Abort.Detail is derived from error text and goes on the wire, and the
// slog attribute-key redactor in internal/logging cannot reach a free string.
func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, redactedMarker)
}
