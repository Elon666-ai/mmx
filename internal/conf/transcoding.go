package conf

// SRTTranscodingConfig is the configuration for SRT transcoding.
type SRTTranscodingConfig struct {
	// Enable transcoding
	Enable bool `json:"enable"`

	// Output configurations
	Outputs []SRTTranscodingOutput `json:"outputs"`
}

// SRTTranscodingOutput is a single transcoding output configuration.
type SRTTranscodingOutput struct {
	// Output path name (used to identify the output)
	Path string `json:"path"`

	// Output type: "video" or "audio"
	Type string `json:"type"`

	// Video configuration (required for video outputs)
	Video *SRTTranscodingVideoConfig `json:"video,omitempty"`

	// Audio configuration (required for audio outputs)
	Audio *SRTTranscodingAudioConfig `json:"audio,omitempty"`
}

// SRTTranscodingVideoConfig is video transcoding configuration.
type SRTTranscodingVideoConfig struct {
	// Resolution in format "WIDTHxHEIGHT" (e.g., "1280x720")
	Resolution string `json:"resolution"`

	// Bitrate in bps (e.g., 1000000 for 1Mbps)
	Bitrate uint `json:"bitrate"`

	// Framerate in fps (e.g., 30)
	Framerate uint `json:"framerate"`

	// FFmpeg preset (e.g., "ultrafast", "veryfast", "fast")
	Preset string `json:"preset"`
}

// SRTTranscodingAudioConfig is audio transcoding configuration.
type SRTTranscodingAudioConfig struct {
	// Bitrate in bps (e.g., 64000 for 64kbps)
	Bitrate uint `json:"bitrate"`

	// Samplerate in Hz (e.g., 48000)
	Samplerate uint `json:"samplerate"`

	// Number of channels (1 or 2)
	Channels uint `json:"channels"`
}
