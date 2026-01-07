package conf

// SimulcastConfig is the configuration for Simulcast WebRTC.
type SimulcastConfig struct {
	// Enable simulcast
	Enable bool `json:"enable"`

	// Input configurations
	Inputs []SimulcastInput `json:"inputs"`
}

// SimulcastInput is a single simulcast input configuration.
type SimulcastInput struct {
	// Input path name (e.g., "live/1080p")
	Path string `json:"path"`

	// Simulcast layer: "high", "medium", "low" (for video only)
	Layer string `json:"layer"`

	// Resolution in format "WIDTHxHEIGHT" (e.g., "1920x1080") (for video only)
	Resolution string `json:"resolution"`

	// Bitrate in bps (e.g., 2000000 for 2Mbps)
	Bitrate uint `json:"bitrate"`

	// Type: "video" or "audio"
	Type string `json:"type"`
}

