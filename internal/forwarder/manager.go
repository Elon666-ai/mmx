package forwarder

import (
	"context"
	"strings"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
)

// Manager manages all forwarders.
type Manager struct {
	forwarders       []Forwarder
	stream           *stream.Stream
	logger           logger.Writer
	ctx              context.Context
	ctxCancel        context.CancelFunc
	writeTimeout     time.Duration
	udpMaxPayloadSize int
}

// NewManager creates a new forwarder manager.
func NewManager(
	ctx context.Context,
	srtTargets []conf.SRTForwardTarget,
	webrtcTargets []conf.WebRTCForwardTarget,
	stream *stream.Stream,
	parent logger.Writer,
	writeTimeout time.Duration,
	udpMaxPayloadSize int,
	udpReadBufferSize uint,
	pathName string,
) *Manager {
	ctx, ctxCancel := context.WithCancel(ctx)

	m := &Manager{
		stream:            stream,
		logger:            parent,
		ctx:               ctx,
		ctxCancel:         ctxCancel,
		writeTimeout:      writeTimeout,
		udpMaxPayloadSize: udpMaxPayloadSize,
	}

	// create SRT forwarders
	for _, target := range srtTargets {
		if !target.Enable {
			continue
		}

		// replace $MTX_PATH variable in URL
		resolvedURL := strings.ReplaceAll(target.URL, "$MTX_PATH", pathName)
		
		// log resolved URL for debugging
		parent.Log(logger.Debug, "SRT forwarder: resolved URL from '%s' to '%s'", target.URL, resolvedURL)

		forwarder := newSRTForwarder(resolvedURL, &target, parent, writeTimeout, udpMaxPayloadSize)
		m.forwarders = append(m.forwarders, forwarder)
	}

	// create WebRTC forwarders
	for _, target := range webrtcTargets {
		if !target.Enable {
			continue
		}

		// replace $MTX_PATH variable in URL
		resolvedURL := strings.ReplaceAll(target.URL, "$MTX_PATH", pathName)
		
		// log resolved URL for debugging
		parent.Log(logger.Debug, "WebRTC forwarder: resolved URL from '%s' to '%s'", target.URL, resolvedURL)

		forwarder := newWebRTCForwarder(resolvedURL, &target, parent, writeTimeout, udpReadBufferSize)
		m.forwarders = append(m.forwarders, forwarder)
	}

	return m
}

// Start starts all forwarders.
func (m *Manager) Start(stream *stream.Stream) {
	m.stream = stream

		for _, f := range m.forwarders {
		go func(forwarder Forwarder) {
			err := forwarder.Start(stream)
			if err != nil {
				m.logger.Log(logger.Warn, "failed to start forwarder %s: %v", forwarder.GetTarget(), err)
			}
		}(f)
	}
}

// Stop stops all forwarders.
func (m *Manager) Stop() {
	m.ctxCancel()

	for _, f := range m.forwarders {
		f.Stop()
	}
}

// GetStats returns statistics for all forwarders.
func (m *Manager) GetStats() []Stats {
	var stats []Stats
	for _, f := range m.forwarders {
		stats = append(stats, f.GetStats())
	}
	return stats
}

