// Package transcoder provides SRT transcoding functionality.
package transcoder

import (
	"context"
	"fmt"
	"sync"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
)

// Manager manages transcoding for a path.
type Manager struct {
	config      *conf.SRTTranscodingConfig
	inputStream *stream.Stream
	outputs     map[string]*Output
	logger      logger.Writer
	ctx         context.Context
	ctxCancel   context.CancelFunc
	wg          sync.WaitGroup

	// State
	active bool
}

// NewManager creates a new transcoder manager.
func NewManager(
	config *conf.SRTTranscodingConfig,
	parent logger.Writer,
) *Manager {
	ctx, ctxCancel := context.WithCancel(context.Background())

	return &Manager{
		config:    config,
		outputs:   make(map[string]*Output),
		logger:    parent,
		ctx:       ctx,
		ctxCancel: ctxCancel,
	}
}

// Start starts transcoding for the given input stream.
func (m *Manager) Start(inputStream *stream.Stream) error {
	if m.active {
		return fmt.Errorf("transcoder already active")
	}

	if m.config == nil || !m.config.Enable {
		m.logger.Log(logger.Debug, "transcoding disabled, skipping")
		return nil
	}

	m.logger.Log(logger.Info, "starting transcoder with %d outputs", len(m.config.Outputs))
	m.inputStream = inputStream
	m.active = true

	// Create and start outputs
	for _, outputConfig := range m.config.Outputs {
		output, err := NewOutput(&outputConfig, m.logger)
		if err != nil {
			return fmt.Errorf("failed to create output %s: %w", outputConfig.Path, err)
		}

		if err := output.Start(inputStream); err != nil {
			return fmt.Errorf("failed to start output %s: %w", outputConfig.Path, err)
		}

		m.outputs[outputConfig.Path] = output
	}

	m.logger.Log(logger.Info, "transcoder started successfully")
	return nil
}

// Stop stops transcoding.
func (m *Manager) Stop() {
	if !m.active {
		return
	}

	m.logger.Log(logger.Info, "stopping transcoder")
	m.active = false
	m.ctxCancel()

	// Stop all outputs
	for name, output := range m.outputs {
		output.Stop()
		m.logger.Log(logger.Debug, "stopped output %s", name)
	}

	m.wg.Wait()
	m.outputs = make(map[string]*Output)
	m.logger.Log(logger.Info, "transcoder stopped")
}

// GetOutputStream returns the output stream for a given path.
func (m *Manager) GetOutputStream(outputPath string) *stream.Stream {
	if output, exists := m.outputs[outputPath]; exists {
		return output.GetStream()
	}
	return nil
}

// IsActive returns whether the transcoder is active.
func (m *Manager) IsActive() bool {
	return m.active
}

