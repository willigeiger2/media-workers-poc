// Package ffmpeg builds ffmpeg argument lists for the media pass-through
// pipeline.
package ffmpeg

import "time"

// ShutdownTimeout is how long we wait for ffmpeg to finalize output after
// the input pipe is closed.
const ShutdownTimeout = 10 * time.Second

// defaultTranscodeBitrate is used when source bitrate is unknown.
const (
	defaultTranscodeBitrate = "2M"
	defaultTranscodeGOP     = "60" // 2s at 30fps
)

// Config describes the stream processing configuration sent by the client
// as a JSON text message before the binary stream begins.
type Config struct {
	// Preset selects a predefined ffmpeg pipeline. If empty, defaults to
	// "passthrough".
	Preset string `json:"preset"`

	// OverlayImage is the path to a static image (e.g. PNG with alpha) used
	// by the "overlay" preset.
	OverlayImage string `json:"overlay_image"`

	// OverlayPosition controls placement. Supported: "top-right" (default),
	// "top-left", "bottom-right", "bottom-left".
	OverlayPosition string `json:"overlay_position"`

	// FFmpegArgs is an escape hatch: if provided, these raw args are used
	// verbatim instead of any preset. Insecure for production, useful for PoC
	// experimentation.
	FFmpegArgs []string `json:"ffmpeg_args"`
}

// BuildArgs returns the full ffmpeg argument list for the given Config.
func BuildArgs(cfg Config, videoInput string, output string) []string {
	if len(cfg.FFmpegArgs) > 0 {
		return cfg.FFmpegArgs
	}

	switch cfg.Preset {
	case "overlay":
		return BuildOverlayArgs(cfg, videoInput, output)
	case "passthrough":
		fallthrough
	default:
		return BuildPassthroughArgs(videoInput, output)
	}
}

// BuildPassthroughArgs constructs ffmpeg arguments for WebSocket pass-through.
//
// Input is a WebM stream (from MediaRecorder) read from a pipe.
// Output is H.264/AAC muxed to FLV for RTMP.
//
// videoInput should be the pipe path, e.g. "/dev/fd/3".
func BuildPassthroughArgs(videoInput string, output string) []string {
	return []string{
		"-y", // Overwrite output without asking.

		// Video input from pipe.
		"-f", "webm",
		"-i", videoInput,

		// Video: transcode to H.264 for RTMP/FLV compatibility.
		"-c:v", "libx264",
		"-preset", "fast",
		"-b:v", defaultTranscodeBitrate,
		"-g", defaultTranscodeGOP,
		"-pix_fmt", "yuv420p", // Ensure compatibility.

		// No audio for Stage 1.
		"-an",

		// Output format and destination.
		"-f", "flv",
		output,
	}
}

// BuildOverlayArgs constructs ffmpeg arguments that composite a static image
// overlay onto the live video stream.
//
// The overlay image is expected to have an alpha channel (e.g. PNG).
// Position defaults to top-right.
func BuildOverlayArgs(cfg Config, videoInput string, output string) []string {
	overlayImage := cfg.OverlayImage
	if overlayImage == "" {
		overlayImage = "/app/overlays/cf-logo.png"
	}

	position := cfg.OverlayPosition
	if position == "" {
		position = "top-right"
	}

	// Compute overlay x:y coordinates based on position.
	// Expression uses W (video width), w (overlay width), H (video height),
	// h (overlay height).
	var xExpr, yExpr string
	switch position {
	case "top-left":
		xExpr = "10"
		yExpr = "10"
	case "bottom-left":
		xExpr = "10"
		yExpr = "H-h-10"
	case "bottom-right":
		xExpr = "W-w-10"
		yExpr = "H-h-10"
	case "top-right":
		fallthrough
	default:
		xExpr = "W-w-10"
		yExpr = "10"
	}

	filterComplex := "[1:v]format=rgba,scale=600:-1[logo];[0:v][logo]overlay=" + xExpr + ":" + yExpr + ":format=auto,format=yuv420p"

	return []string{
		"-y",

		// Input 0: live video from browser (WebM/VP8).
		"-f", "webm",
		"-i", videoInput,

		// Input 1: overlay image.
		"-i", overlayImage,

		// Composite overlay on top of video.
		"-filter_complex", filterComplex,

		// Video: transcode to H.264 for RTMP/FLV compatibility.
		"-c:v", "libx264",
		"-preset", "fast",
		"-b:v", defaultTranscodeBitrate,
		"-g", defaultTranscodeGOP,
		"-pix_fmt", "yuv420p",

		// No audio.
		"-an",

		// Output format and destination.
		"-f", "flv",
		output,
	}
}
