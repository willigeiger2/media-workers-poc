// Package ffmpeg builds ffmpeg argument lists for the media pass-through
// pipeline.
package ffmpeg

import (
	"fmt"
	"strings"
	"time"
)

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

	// StreamURL overrides the RTMP destination. If empty, uses the
	// STREAM_URL environment variable.
	StreamURL string `json:"stream_url"`

	// SourceType selects the input source. "webcam" (default) reads binary
	// chunks from the WebSocket. "stream" reads an HLS manifest directly.
	SourceType string `json:"source_type"`

	// VideoURL is the HLS manifest URL when SourceType is "stream".
	VideoURL string `json:"video_url"`

	// BurnSubtitles enables subtitle burn-in when SourceType is "stream".
	// ffmpeg reads the subtitle file and overlays text onto video frames.
	BurnSubtitles bool `json:"burn_subtitles"`

	// SubtitleFile is the path to the SRT subtitle file on disk. When empty
	// and BurnSubtitles is true, the container will fetch and convert VTT.
	SubtitleFile string `json:"subtitle_file"`

	// OverlayImage is the path to a static image (e.g. PNG with alpha) used
	// by the "overlay" preset.
	OverlayImage string `json:"overlay_image"`

	// OverlayPosition controls placement. Supported: "top-right" (default),
	// "top-left", "bottom-right", "bottom-left".
	OverlayPosition string `json:"overlay_position"`

	// PreviewMode sends the transcoded output back over the WebSocket
	// instead of pushing it to an RTMP destination. This gives a low-latency
	// browser preview using MediaSource Extensions.
	PreviewMode bool `json:"preview_mode"`

	// OverlayWidth and OverlayHeight specify the dimensions of the raw
	// RGBA overlay stream (animated-overlay preset).
	OverlayWidth  int `json:"overlay_width"`
	OverlayHeight int `json:"overlay_height"`

	// TimezoneOffsetMin is the client's timezone offset from UTC in minutes
	// (e.g., -420 for PDT). Used by the animated-overlay preset to display
	// local time.
	TimezoneOffsetMin int `json:"timezone_offset_min"`

	// Filters holds real-time video filter parameters (filters preset).
	Filters FilterParams `json:"filters"`

	// FFmpegArgs is an escape hatch: if provided, these raw args are used
	// verbatim instead of any preset. Only available when built with the
	// "allow_raw_ffmpeg_args" build tag. Insecure for production.
	FFmpegArgs []string `json:"ffmpeg_args"`
}

// FilterParams holds the adjustable video filter values.
type FilterParams struct {
	Blur       float64 `json:"blur"`       // 0–10
	Brightness float64 `json:"brightness"` // -1.0 to 1.0
	Contrast   float64 `json:"contrast"`   // 0.0 to 2.0
	Saturation float64 `json:"saturation"` // 0.0 to 2.0
	Gamma      float64 `json:"gamma"`      // 0.1 to 3.0
	Sharpen    float64 `json:"sharpen"`    // 0–5
	Flip       bool    `json:"flip"`       // horizontal flip
	Rotate     int     `json:"rotate"`     // 0, 90, 180, 270
}

// BuildArgs returns the full ffmpeg argument list for the given Config.
func BuildArgs(cfg Config, videoInput string, output string) []string {
	if rawArgsEnabled() && len(cfg.FFmpegArgs) > 0 {
		return cfg.FFmpegArgs
	}

	// Determine input source. videoInput is either the HLS URL (for direct
	// ffmpeg HLS input) or a pipe path (/dev/fd/3 for segment downloader).
	input := videoInput

	switch cfg.Preset {
	case "animated-overlay":
		return BuildAnimatedOverlayArgs(cfg, "/dev/fd/3", output)
	case "filters":
		return BuildFilterArgs(cfg, input, output)
	case "overlay":
		return BuildOverlayArgs(cfg, input, output)
	case "passthrough":
		fallthrough
	default:
		return BuildPassthroughArgs(cfg, input, output)
	}
}

// BuildPassthroughArgs constructs ffmpeg arguments for WebSocket pass-through.
//
// Input is a WebM stream (from MediaRecorder) read from a pipe, or an HLS
// manifest URL when source_type is "stream".
// Output is H.264/AAC muxed to FLV for RTMP, or fragmented MP4 to stdout
// when preview mode is enabled.
func BuildPassthroughArgs(cfg Config, videoInput string, output string) []string {
	isStream := cfg.SourceType == "stream"

	args := []string{
		"-y", // Overwrite output without asking.
	}

	if isStream {
		// Stream source: segments fed through a pipe from the Go
		// hlsdownloader.  The downloader paces writes at real-time speed,
		// so we do NOT use -re here (it is unreliable with pipe input).
		// Use -copyts to preserve original presentation timestamps.
		args = append(args,
			"-copyts",
			"-i", videoInput,
		)
	} else {
		// Webcam source: raw WebM chunks from browser MediaRecorder.
		args = append(args, "-f", "webm", "-i", videoInput)
	}

	// Apply subtitle burn-in if requested and file exists.
	filterComplex := buildSubtitleFilter(cfg, "")
	if filterComplex != "" {
		args = append(args, "-filter_complex", filterComplex)
	}

	args = append(args,
		// Video: transcode to H.264 for RTMP/FLV compatibility.
		"-c:v", "libx264",
		"-preset", "fast",
		"-b:v", defaultTranscodeBitrate,
		"-g", defaultTranscodeGOP,
		"-pix_fmt", "yuv420p", // Ensure compatibility.

		// No audio for Stage 1.
		"-an",
	)

	if cfg.PreviewMode {
		args = append(args,
			"-crf", "20",
			"-f", "mp4",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"pipe:1",
		)
	} else {
		args = append(args,
			"-f", "flv",
			output,
		)
	}

	return args
}

// buildSubtitleFilter returns a subtitle filter string if burn-in is enabled
// and a subtitle file is available. If baseFilter is non-empty, the subtitle
// filter is appended to it with a comma separator.
func buildSubtitleFilter(cfg Config, baseFilter string) string {
	if !cfg.BurnSubtitles || cfg.SubtitleFile == "" {
		return baseFilter
	}

	subFilter := fmt.Sprintf("subtitles=%s:force_style='BackColour=&H80000000,BorderStyle=3,Outline=4,Shadow=0'", cfg.SubtitleFile)
	if baseFilter != "" {
		return baseFilter + "," + subFilter
	}
	return subFilter
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

	isStream := cfg.SourceType == "stream"

	filterComplex := "[1:v]format=rgba,scale=600:-1[logo];[0:v][logo]overlay=" + xExpr + ":" + yExpr + ":format=auto,format=yuv420p"
	filterComplex = buildSubtitleFilter(cfg, filterComplex)

	args := []string{
		"-y",
	}

	if isStream {
		// Input 0: stream video (from hlsdownloader pipe).
		args = append(args,
			"-copyts",
			"-i", videoInput,
		)
	} else {
		// Input 0: live video from browser (WebM/VP8).
		args = append(args, "-f", "webm", "-i", videoInput)
	}

	args = append(args,
		// Input 1: overlay image.
		"-i", overlayImage,

		// Composite overlay on top of video.
		"-filter_complex", filterComplex,
	)

	args = append(args,
		// Video: transcode to H.264 for RTMP/FLV compatibility.
		"-c:v", "libx264",
		"-preset", "fast",
		"-b:v", defaultTranscodeBitrate,
		"-g", defaultTranscodeGOP,
		"-pix_fmt", "yuv420p",

		// No audio.
		"-an",
	)

	if cfg.PreviewMode {
		args = append(args,
			"-crf", "20",
			"-f", "mp4",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"pipe:1",
		)
	} else {
		args = append(args,
			"-f", "flv",
			output,
		)
	}

	return args
}

// BuildAnimatedOverlayArgs constructs ffmpeg arguments that composite a
// real-time RGBA overlay stream (generated by Go) onto the live video.
//
// Input 0: live video from browser (WebM/VP8) or stream source (fMP4) via videoInput pipe.
// Input 1: raw RGBA overlay frames via /dev/fd/4 pipe.
func BuildAnimatedOverlayArgs(cfg Config, videoInput string, output string) []string {
	w := cfg.OverlayWidth
	if w == 0 {
		w = 1280
	}
	h := cfg.OverlayHeight
	if h == 0 {
		h = 720
	}

	isStream := cfg.SourceType == "stream"

	// Base video encoding args.
	videoArgs := []string{
		"-c:v", "libx264",
		"-preset", "fast",
		"-b:v", defaultTranscodeBitrate,
		"-g", defaultTranscodeGOP,
		"-pix_fmt", "yuv420p",
		"-an",
	}

	// Output format depends on preview mode.
	if cfg.PreviewMode {
		videoArgs = append(videoArgs,
			"-f", "mp4",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"pipe:1",
		)
	} else {
		videoArgs = append(videoArgs,
			"-f", "flv",
			output,
		)
	}

	args := []string{
		"-y",
	}

	if isStream {
		// Stream source: segments fed through pipe.
		args = append(args,
			"-copyts",
			"-i", videoInput,
		)
	} else {
		// Webcam source: raw WebM chunks from browser MediaRecorder.
		args = append(args,
			"-f", "webm",
			"-i", videoInput,
		)
	}

	args = append(args,
		// Input 1: raw RGBA overlay from Go frame generator.
		"-f", "rawvideo",
		"-pix_fmt", "rgba",
		"-s", fmt.Sprintf("%dx%d", w, h),
		"-r", "30",
		"-i", "/dev/fd/4",

		// Composite overlay on top of video.
		"-filter_complex", "[0:v][1:v]overlay=0:0:format=auto",
	)

	return append(args, videoArgs...)
}

// BuildFilterArgs constructs ffmpeg arguments with a dynamic filter chain.
// Supported filters: blur, brightness, contrast, saturation, gamma, sharpen,
// flip, rotate. Also supports subtitle burn-in for stream sources.
func BuildFilterArgs(cfg Config, videoInput string, output string) []string {
	f := cfg.Filters
	isStream := cfg.SourceType == "stream"

	// Build the filter_complex chain.
	var filters []string

	// eq filter: brightness, contrast, saturation, gamma.
	var eqParts []string
	if f.Brightness != 0 {
		eqParts = append(eqParts, fmt.Sprintf("brightness=%.2f", f.Brightness))
	}
	if f.Contrast != 1.0 {
		eqParts = append(eqParts, fmt.Sprintf("contrast=%.2f", f.Contrast))
	}
	if f.Saturation != 1.0 {
		eqParts = append(eqParts, fmt.Sprintf("saturation=%.2f", f.Saturation))
	}
	if f.Gamma != 1.0 {
		eqParts = append(eqParts, fmt.Sprintf("gamma=%.2f", f.Gamma))
	}
	if len(eqParts) > 0 {
		filters = append(filters, "eq="+strings.Join(eqParts, ":"))
	}

	// boxblur.
	if f.Blur > 0 {
		filters = append(filters, fmt.Sprintf("boxblur=%.1f:1", f.Blur))
	}

	// unsharp (sharpen).
	if f.Sharpen > 0 {
		filters = append(filters, fmt.Sprintf("unsharp=3:3:%.1f", f.Sharpen))
	}

	// hflip.
	if f.Flip {
		filters = append(filters, "hflip")
	}

	// transpose (rotate).
	switch f.Rotate {
	case 90:
		filters = append(filters, "transpose=1")
	case 180:
		filters = append(filters, "transpose=2,transpose=2")
	case 270:
		filters = append(filters, "transpose=2")
	}

	// Ensure yuv420p output for compatibility.
	filters = append(filters, "format=yuv420p")

	filterComplex := strings.Join(filters, ",")
	filterComplex = buildSubtitleFilter(cfg, filterComplex)

	// Base video encoding args.
	videoArgs := []string{
		"-c:v", "libx264",
		"-preset", "fast",
		"-b:v", defaultTranscodeBitrate,
		"-g", defaultTranscodeGOP,
		"-pix_fmt", "yuv420p",
		"-an",
	}

	// Output format depends on preview mode.
	if cfg.PreviewMode {
		videoArgs = append(videoArgs,
			"-crf", "20",
			"-f", "mp4",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"pipe:1",
		)
	} else {
		videoArgs = append(videoArgs,
			"-f", "flv",
			output,
		)
	}

	args := []string{"-y"}

	if isStream {
		// Input 0: stream video (from hlsdownloader pipe).
		args = append(args,
			"-copyts",
			"-i", videoInput,
		)
	} else {
		args = append(args, "-f", "webm", "-i", videoInput)
	}

	args = append(args, "-filter_complex", filterComplex)

	// Base video encoding args.
	args = append(args,
		"-c:v", "libx264",
		"-preset", "fast",
		"-b:v", defaultTranscodeBitrate,
		"-g", defaultTranscodeGOP,
		"-pix_fmt", "yuv420p",
		"-an",
	)

	args = append(args, videoArgs...)

	return args
}
