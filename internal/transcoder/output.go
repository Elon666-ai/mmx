package transcoder

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/mpegts"
	"github.com/bluenviron/mediamtx/internal/stream"
)

// Output represents a single transcoding output.
type Output struct {
	config     *conf.SRTTranscodingOutput
	stream     *stream.Stream
	process    *exec.Cmd
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	stderr     io.ReadCloser
	reader     *stream.Reader
	logger     logger.Writer
	ctx        context.Context
	ctxCancel  context.CancelFunc
	wg         sync.WaitGroup

	// State
	active bool
}

// NewOutput creates a new transcoding output.
func NewOutput(
	config *conf.SRTTranscodingOutput,
	parent logger.Writer,
) (*Output, error) {
	ctx, ctxCancel := context.WithCancel(context.Background())

	// Create output stream
	strm := &stream.Stream{
		WriteQueueSize:     64, // Small buffer for low latency
		RTPMaxPayloadSize:  1460,
		Desc:               createStreamDescription(config),
		GenerateRTPPackets: true,
		FillNTP:            true,
		Parent:             parent,
	}

	if err := strm.Initialize(); err != nil {
		return nil, fmt.Errorf("failed to initialize stream: %w", err)
	}

	return &Output{
		config:    config,
		stream:    strm,
		logger:    parent,
		ctx:       ctx,
		ctxCancel: ctxCancel,
	}, nil
}

// Start starts the transcoding output.
func (o *Output) Start(inputStream *stream.Stream) error {
	if o.active {
		return fmt.Errorf("output already active")
	}

	o.logger.Log(logger.Info, "starting transcoding output %s", o.config.Path)

	// Build FFmpeg command
	args := o.buildFFmpegArgs()
	o.process = exec.CommandContext(o.ctx, "ffmpeg", args...)

	// Setup pipes
	var err error
	o.stdin, err = o.process.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	o.stdout, err = o.process.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	o.stderr, err = o.process.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// Start FFmpeg process
	if err := o.process.Start(); err != nil {
		return fmt.Errorf("failed to start FFmpeg: %w", err)
	}

	o.active = true

	// Start data processing goroutines
	o.wg.Add(3)
	go o.inputProcessor(inputStream)
	go o.outputProcessor()
	go o.stderrMonitor()

	o.logger.Log(logger.Info, "transcoding output %s started", o.config.Path)
	return nil
}

// Stop stops the transcoding output.
func (o *Output) Stop() {
	if !o.active {
		return
	}

	o.logger.Log(logger.Info, "stopping transcoding output %s", o.config.Path)
	o.active = false
	o.ctxCancel()

	// Close stdin to signal EOF to FFmpeg
	if o.stdin != nil {
		o.stdin.Close()
	}

	// Wait for goroutines
	o.wg.Wait()

	// Kill FFmpeg process if still running
	if o.process != nil && o.process.Process != nil {
		done := make(chan bool, 1)
		go func() {
			o.process.Wait()
			done <- true
		}()

		select {
		case <-done:
			// Process exited gracefully
		case <-time.After(2 * time.Second):
			// Force kill
			o.process.Process.Kill()
			o.process.Wait()
		}
	}

	o.logger.Log(logger.Info, "transcoding output %s stopped", o.config.Path)
}

// GetStream returns the output stream.
func (o *Output) GetStream() *stream.Stream {
	return o.stream
}

// inputProcessor processes input data and sends to FFmpeg.
func (o *Output) inputProcessor(inputStream *stream.Stream) {
	defer o.wg.Done()

	// Create reader
	o.reader = &stream.Reader{Parent: o.logger}
	inputStream.AddReader(o.reader)
	defer inputStream.RemoveReader(o.reader)

	// Create MPEG-TS writer
	bw := bufio.NewWriter(o.stdin)
	defer bw.Flush()

	// Use existing MPEG-TS conversion logic
	err := mpegts.FromStream(
		inputStream.Desc,
		o.reader,
		bw,
		nil, // No SRT connection needed
		time.Second,
	)

	if err != nil && err != io.EOF {
		o.logger.Log(logger.Warn, "input processor error: %v", err)
	}
}

// outputProcessor processes FFmpeg output and writes to stream.
func (o *Output) outputProcessor() {
	defer o.wg.Done()

	// Create enhanced MPEG-TS reader
	enhancedReader := &mpegts.EnhancedReader{R: o.stdout}
	if err := enhancedReader.Initialize(); err != nil {
		// EOF is expected when FFmpeg closes, don't log it as error during shutdown
		if err == io.EOF {
			select {
			case <-o.ctx.Done():
				o.logger.Log(logger.Debug, "MPEG-TS reader closed (FFmpeg terminated)")
				return
			default:
				o.logger.Log(logger.Warn, "MPEG-TS reader got EOF before FFmpeg started: %v", err)
			}
		} else {
			o.logger.Log(logger.Error, "failed to initialize MPEG-TS reader: %v", err)
		}
		return
	}

	// Convert MPEG-TS to stream
	// ToStream expects a pointer to stream pointer
	var streamPtr *stream.Stream = o.stream
	medias, err := mpegts.ToStream(enhancedReader, &streamPtr, o.logger)
	if err != nil && err != io.EOF {
		o.logger.Log(logger.Warn, "failed to convert MPEG-TS to stream: %v, keeping initial description", err)
		// Don't return here, keep the initial description and continue reading
	} else if len(medias) > 0 {
		// Update stream description with actual medias from FFmpeg
		o.stream.Desc = &description.Session{Medias: medias}
		o.logger.Log(logger.Info, "updated stream description with %d medias from FFmpeg output", len(medias))
	} else {
		o.logger.Log(logger.Debug, "no medias found in FFmpeg output, keeping initial description")
	}

	// Start reading from the enhanced reader in a goroutine
	readErr := make(chan error, 1)
	go func() {
		for {
			err := enhancedReader.Read()
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	// Wait for context cancellation or read error
	select {
	case <-o.ctx.Done():
		// Context cancelled, stop reading
		return
	case err := <-readErr:
		if err != nil && err != io.EOF {
			o.logger.Log(logger.Warn, "MPEG-TS reader error: %v", err)
		}
		return
	}
}

// stderrMonitor monitors FFmpeg stderr for errors.
func (o *Output) stderrMonitor() {
	defer o.wg.Done()

	scanner := bufio.NewScanner(o.stderr)
	for scanner.Scan() {
		line := scanner.Text()
		o.logger.Log(logger.Debug, "FFmpeg: %s", line)
	}
}

// buildFFmpegArgs builds FFmpeg command line arguments.
func (o *Output) buildFFmpegArgs() []string {
	args := []string{
		"-f", "mpegts",
		"-i", "pipe:0",
	}

	// Video encoding
	if o.config.Type == "video" && o.config.Video != nil {
		args = append(args,
			"-c:v", "libx264",
			"-preset", o.config.Video.Preset,
			"-tune", "zerolatency",
			"-b:v", fmt.Sprintf("%dk", o.config.Video.Bitrate/1000),
			"-s", o.config.Video.Resolution,
			"-r", fmt.Sprintf("%d", o.config.Video.Framerate),
			"-g", fmt.Sprintf("%d", o.config.Video.Framerate*2),
			"-keyint_min", fmt.Sprintf("%d", o.config.Video.Framerate*2),
			"-bf", "0",
			"-pix_fmt", "yuv420p",
		)
	} else {
		args = append(args, "-vn")
	}

	// Audio encoding
	if o.config.Type == "audio" && o.config.Audio != nil {
		args = append(args,
			"-c:a", "libopus",
			"-b:a", fmt.Sprintf("%dk", o.config.Audio.Bitrate/1000),
			"-ar", fmt.Sprintf("%d", o.config.Audio.Samplerate),
			"-ac", "2",
		)
	} else if o.config.Type == "video" {
		args = append(args,
			"-c:a", "libopus",
			"-b:a", "64k",
			"-ar", "48000",
			"-ac", "2",
		)
	}

	// Output configuration
	args = append(args,
		"-f", "mpegts",
		"-fflags", "+discardcorrupt+genpts+nobuffer",
		"-max_delay", "100000",
		"-avoid_negative_ts", "make_zero",
		"pipe:1",
	)

	return args
}

// createStreamDescription creates stream description for output.
func createStreamDescription(config *conf.SRTTranscodingOutput) *description.Session {
	desc := &description.Session{}

	if config.Type == "video" {
		videoTrack := &description.Media{
			Type: description.MediaTypeVideo,
			Formats: []format.Format{
				&format.H264{
					PayloadTyp:        96,
					SPS:               []byte{0x67, 0x42, 0xc0, 0x28, 0xd9, 0x00, 0x78, 0x02, 0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc9, 0x20},
					PPS:               []byte{0x08, 0x06, 0x07, 0x08},
					PacketizationMode: 1,
				},
			},
		}
		desc.Medias = append(desc.Medias, videoTrack)
	}

	audioTrack := &description.Media{
		Type: description.MediaTypeAudio,
		Formats: []format.Format{
			&format.Opus{
				PayloadTyp:   97,
				ChannelCount: 2,
			},
		},
	}
	desc.Medias = append(desc.Medias, audioTrack)

	return desc
}

