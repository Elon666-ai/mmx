package forwarder

import (
	"github.com/bluenviron/mediamtx/internal/stream"
)

// Forwarder is the interface that all forwarders must implement.
type Forwarder interface {
	// Start starts the forwarder.
	Start(stream *stream.Stream) error

	// Stop stops the forwarder.
	Stop()

	// IsRunning returns whether the forwarder is running.
	IsRunning() bool

	// GetStats returns statistics.
	GetStats() Stats

	// GetTarget returns the target URL.
	GetTarget() string
}

// Stats contains forwarder statistics.
type Stats struct {
	BytesSent     uint64
	PacketsSent   uint64
	PacketsLost   uint64
	LastError     error
	Connected     bool
	ReconnectCount uint64
}

