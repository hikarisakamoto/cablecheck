package app

import (
	"cablecheck/internal/model"
	"cablecheck/internal/protocol"
)

const (
	// ProtocolVersion is the current peer protocol version.
	ProtocolVersion = protocol.Version
	// SchemaVersion is the current report schema version.
	SchemaVersion = model.SchemaVersion
)
