package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/gorilla/websocket"
	"media-workers-poc/ffmpeg"
	"media-workers-poc/hlsdownloader"
	"media-workers-poc/overlay"
	"media-workers-poc/subtitle"
)

// StreamHandler manages a single WebSocket connection and its ffmpeg pipeline.
type StreamHandler struct {
	conn   *websocket.Conn
	logger *log.Logger
}

// NewStreamHandler creates a new handler for the given WebSocket connection.
func NewStreamHandler(conn *websocket.Conn) *StreamHandler {
	return &StreamHandler{
		conn:   conn,
		logger: log.New(os.Stderr, "[handler] ", log.LstdFlags|log.Lmsgprefix),
	}
}

// Run blocks until the stream is finished. It spawns ffmpeg and proxies
// WebSocket messages to ffmpeg's stdin pipe (webcam source) or lets ffmpeg
// read an HLS URL directly (stream source).
func (h *StreamHandler) Run() error {
	streamURL := os.Getenv("STREAM_URL")

	// Step 1: Read configuration and first binary chunk.
	cfg, firstChunk, err := h.readConfigAndFirstChunk()
	if err != nil {
		return fmt.Errorf("failed to read stream config: %w", err)
	}

	// Allow client to override the stream destination.
	if cfg.StreamURL != "" {
		streamURL = cfg.StreamURL
	}

	// Validate that we have a destination unless we're in preview mode.
	if !cfg.PreviewMode && streamURL == "" {
		return fmt.Errorf("STREAM_URL environment variable not set")
	}

	isStreamSource := cfg.SourceType == "stream"
	isAnimatedOverlay := cfg.Preset == "animated-overlay"
	isFilters := cfg.Preset == "filters"

	h.logger.Printf("Config: preset=%q source=%q preview=%v animated=%v filters=%v stream=%s video=%s",
		cfg.Preset, cfg.SourceType, cfg.PreviewMode, isAnimatedOverlay, isFilters, streamURL, cfg.VideoURL)

	if isStreamSource {
		return h.runStreamSource(cfg, streamURL)
	}

	return h.runWebcamSource(cfg, firstChunk, streamURL)
}

// runStreamSource handles streaming from an HLS URL.  Segments are downloaded
// by the hlsdownloader package and fed to ffmpeg via a pipe, giving us full
// control over buffering and pacing.
func (h *StreamHandler) runStreamSource(cfg ffmpeg.Config, streamURL string) error {
	// If subtitle burn-in is requested, fetch and convert subtitles before
	// starting ffmpeg.
	if cfg.BurnSubtitles {
		videoID := extractVideoID(cfg.VideoURL)
		if videoID != "" {
			h.logger.Printf("Fetching subtitles for video ID: %s", videoID)
			result, err := subtitle.FetchAndConvert(videoID)
			if err != nil {
				h.logger.Printf("Subtitle fetch failed: %v", err)
			} else if result.Warning != "" {
				h.logger.Printf("Subtitle warning: %s", result.Warning)
				// Send warning to browser.
				h.conn.WriteMessage(websocket.TextMessage, []byte(`{"warning":"`+result.Warning+`"}`))
			} else {
				h.logger.Printf("Subtitles ready: lang=%s cues=%d file=%s",
					result.Language, result.CueCount, result.SubtitleFile)
				cfg.SubtitleFile = result.SubtitleFile
				// Send subtitle info to browser.
				info := fmt.Sprintf(`{"subtitle_lang":"%s","subtitle_cues":%d}`,
					result.Language, result.CueCount)
				h.conn.WriteMessage(websocket.TextMessage, []byte(info))
			}
		}
	}

	isAnimatedOverlay := cfg.Preset == "animated-overlay"

	// Step 1: Create a pipe for ffmpeg video input.
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to create pipe: %w", err)
	}

	// Step 1b: For animated overlay, create a second pipe and start frame generator.
	var overlayPr, overlayPw *os.File
	var frameGenDone chan error
	var genDone chan struct{}
	if isAnimatedOverlay {
		overlayPr, overlayPw, err = os.Pipe()
		if err != nil {
			pr.Close()
			pw.Close()
			return fmt.Errorf("failed to create overlay pipe: %w", err)
		}

		gen := overlay.NewFrameGenerator()
		gen.Width = cfg.OverlayWidth
		if gen.Width == 0 {
			gen.Width = 1280
		}
		gen.Height = cfg.OverlayHeight
		if gen.Height == 0 {
			gen.Height = 720
		}
		gen.TimezoneOffsetMin = cfg.TimezoneOffsetMin
		genDone = make(chan struct{})
		frameGenDone = make(chan error, 1)
		go func() {
			frameGenDone <- gen.Run(overlayPw, genDone)
		}()
	}

	// Step 2: Start HLS segment downloader goroutine.
	downloaderDone := make(chan error, 1)
	go func() {
		h.logger.Printf("Starting HLS downloader for: %s", cfg.VideoURL)
		err := hlsdownloader.DownloadAndStream(cfg.VideoURL, pw)
		if err != nil {
			h.logger.Printf("HLS downloader error: %v", err)
		}
		downloaderDone <- err
	}()

	// Step 3: Build ffmpeg arguments and start ffmpeg.
	var args []string
	if isAnimatedOverlay {
		args = ffmpeg.BuildAnimatedOverlayArgs(cfg, "/dev/fd/3", streamURL)
	} else {
		args = ffmpeg.BuildArgs(cfg, "/dev/fd/3", streamURL)
	}
	h.logger.Printf("Starting ffmpeg (stream source via pipe): %v", args)

	cmd := exec.Command("ffmpeg", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if isAnimatedOverlay {
		cmd.ExtraFiles = []*os.File{pr, overlayPr}
	} else {
		cmd.ExtraFiles = []*os.File{pr}
	}
	cmd.Stderr = os.Stderr

	var ffmpegOut io.ReadCloser
	if cfg.PreviewMode {
		ffmpegOut, err = cmd.StdoutPipe()
		if err != nil {
			pr.Close()
			if overlayPr != nil {
				overlayPr.Close()
			}
			return fmt.Errorf("failed to create stdout pipe: %w", err)
		}
	} else {
		cmd.Stdout = os.Stdout
	}

	if err := cmd.Start(); err != nil {
		pr.Close()
		if overlayPr != nil {
			overlayPr.Close()
		}
		return fmt.Errorf("failed to start ffmpeg: %w", err)
	}

	// Close read ends in parent — only ffmpeg reads them.
	pr.Close()
	if overlayPr != nil {
		overlayPr.Close()
	}

	// In preview mode, proxy ffmpeg stdout back to the WebSocket.
	var previewDone chan error
	if cfg.PreviewMode && ffmpegOut != nil {
		previewDone = make(chan error, 1)
		go func() {
			previewDone <- h.proxyFfmpegOutputToWebSocket(ffmpegOut)
		}()
	}

	// Wait for ffmpeg exit or client disconnect.
	ffmpegDone := make(chan error, 1)
	go func() {
		ffmpegDone <- cmd.Wait()
	}()

	// Also watch for WebSocket close so we can kill ffmpeg when the
	// browser disconnects.
	wsClose := make(chan error, 1)
	go func() {
		_, _, err := h.conn.ReadMessage()
		if err != nil {
			wsClose <- err
		}
	}()

	select {
	case err := <-ffmpegDone:
		if isAnimatedOverlay {
			close(genDone)
		}
		if err != nil {
			h.logger.Printf("ffmpeg exited with error: %v", err)
			h.conn.Close()
			return fmt.Errorf("ffmpeg failed: %w", err)
		}
		h.logger.Printf("ffmpeg exited cleanly")
		return nil

	case <-wsClose:
		h.logger.Printf("WebSocket closed, killing ffmpeg")
		if isAnimatedOverlay {
			close(genDone)
		}
		cmd.Process.Kill()
		<-ffmpegDone
		return nil
	}
}

// runWebcamSource handles streaming from the browser webcam via WebSocket
// binary chunks piped to ffmpeg.
func (h *StreamHandler) runWebcamSource(cfg ffmpeg.Config, firstChunk []byte, streamURL string) error {
	isAnimatedOverlay := cfg.Preset == "animated-overlay"

	// Start a persistent goroutine that reads from the WebSocket and feeds
	// binary chunks into a buffered channel.
	chunkQueue := make(chan []byte, 100)
	wsDone := make(chan error, 1)
	go func() {
		wsDone <- h.wsReader(chunkQueue)
	}()

	// Seed the first chunk into the queue.
	chunkQueue <- firstChunk

	// Main loop: manages ffmpeg lifecycle. Each iteration starts a new ffmpeg
	// process. For the filters preset, the browser now does a full reconnect
	// when filters change, so each connection gets exactly one ffmpeg process.
	for iteration := 0; ; iteration++ {
		// Sanity check — we only expect one iteration per connection now.
		if iteration > 0 {
			h.logger.Printf("Unexpected iteration %d — client should reconnect for filter changes", iteration)
		}

		// Step 2: Create a pipe for ffmpeg video input.
		pr, pw, err := os.Pipe()
		if err != nil {
			return fmt.Errorf("failed to create pipe: %w", err)
		}

		// Step 2b: For animated overlay, create a second pipe and start frame generator.
		var overlayPr, overlayPw *os.File
		var frameGenDone chan error
		var genDone chan struct{}
		if isAnimatedOverlay {
			overlayPr, overlayPw, err = os.Pipe()
			if err != nil {
				pr.Close()
				pw.Close()
				return fmt.Errorf("failed to create overlay pipe: %w", err)
			}

			gen := overlay.NewFrameGenerator()
			gen.Width = cfg.OverlayWidth
			if gen.Width == 0 {
				gen.Width = 1280
			}
			gen.Height = cfg.OverlayHeight
			if gen.Height == 0 {
				gen.Height = 720
			}
			gen.TimezoneOffsetMin = cfg.TimezoneOffsetMin
			genDone = make(chan struct{})
			frameGenDone = make(chan error, 1)
			go func() {
				frameGenDone <- gen.Run(overlayPw, genDone)
			}()
		}

		// Step 3: Build ffmpeg arguments and start ffmpeg.
		var args []string
		if isAnimatedOverlay {
			args = ffmpeg.BuildAnimatedOverlayArgs(cfg, "/dev/fd/3", streamURL)
		} else {
			args = ffmpeg.BuildArgs(cfg, "/dev/fd/3", streamURL)
		}
		h.logger.Printf("Starting ffmpeg (iteration %d): %v", iteration, args)

		cmd := exec.Command("ffmpeg", args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if isAnimatedOverlay {
			cmd.ExtraFiles = []*os.File{pr, overlayPr}
		} else {
			cmd.ExtraFiles = []*os.File{pr}
		}
		cmd.Stderr = os.Stderr

		var ffmpegOut io.ReadCloser
		if cfg.PreviewMode {
			ffmpegOut, err = cmd.StdoutPipe()
			if err != nil {
				pr.Close()
				pw.Close()
				if overlayPr != nil {
					overlayPr.Close()
				}
				return fmt.Errorf("failed to create stdout pipe: %w", err)
			}
		} else {
			cmd.Stdout = os.Stdout
		}

		if err := cmd.Start(); err != nil {
			pr.Close()
			pw.Close()
			if overlayPr != nil {
				overlayPr.Close()
			}
			return fmt.Errorf("failed to start ffmpeg: %w", err)
		}

		// Close read ends in parent — only ffmpeg reads them.
		pr.Close()
		if overlayPr != nil {
			overlayPr.Close()
		}

		// Step 4: Start a goroutine that drains chunkQueue and writes to the pipe.
		stopWriter := make(chan struct{})
		writerDone := make(chan error, 1)
		go func(w io.WriteCloser) {
			writerDone <- h.pipeWriter(w, chunkQueue, stopWriter)
		}(pw)

		// Step 5: In preview mode, proxy ffmpeg stdout back to the WebSocket.
		var previewDone chan error
		if cfg.PreviewMode && ffmpegOut != nil {
			previewDone = make(chan error, 1)
			go func() {
				previewDone <- h.proxyFfmpegOutputToWebSocket(ffmpegOut)
			}()
		}

		// Step 6: Wait for ffmpeg exit or client disconnect.
		ffmpegDone := make(chan error, 1)
		go func() {
			ffmpegDone <- cmd.Wait()
		}()

		select {
		case err := <-writerDone:
			if err != nil {
				h.logger.Printf("Pipe writer error: %v", err)
			}
			// Pipe closed (client disconnected or error).
			cmd.Process.Kill()
			<-ffmpegDone
			if isAnimatedOverlay && genDone != nil {
				close(genDone)
				<-frameGenDone
			}
			return nil

		case err := <-ffmpegDone:
			// ffmpeg exited early (e.g., malformed args) or cleanly.
			if err != nil {
				h.logger.Printf("ffmpeg exited with error: %v", err)
			} else {
				h.logger.Printf("ffmpeg exited cleanly")
			}
			close(stopWriter)
			<-writerDone
			if isAnimatedOverlay && genDone != nil {
				close(genDone)
				<-frameGenDone
			}
			if err != nil {
				h.conn.Close()
				return fmt.Errorf("ffmpeg failed: %w", err)
			}
			return nil
		}
	}
}

// readConfigAndFirstChunk reads messages from the WebSocket until it has
// both a Config (optional) and the first binary chunk (required for webcam
// source). For stream source, no binary chunk is expected.
func (h *StreamHandler) readConfigAndFirstChunk() (ffmpeg.Config, []byte, error) {
	var cfg ffmpeg.Config
	var firstChunk []byte

	for {
		messageType, data, err := h.conn.ReadMessage()
		if err != nil {
			return cfg, nil, fmt.Errorf("failed to read WebSocket message: %w", err)
		}

		if messageType == websocket.TextMessage {
			if err := json.Unmarshal(data, &cfg); err != nil {
				h.logger.Printf("Invalid config JSON, using defaults: %v", err)
				cfg = ffmpeg.Config{}
			}
			// For stream source, the config is all we need — no binary chunks
			// will arrive. Return immediately.
			if cfg.SourceType == "stream" {
				return cfg, nil, nil
			}
			continue
		}

		if messageType == websocket.BinaryMessage {
			firstChunk = data
			break
		}
	}

	return cfg, firstChunk, nil
}

// wsReader reads messages from the WebSocket forever. Binary chunks are sent
// to chunkQueue. Text messages are ignored (the initial config is read by
// readConfigAndFirstChunk before this goroutine starts).
func (h *StreamHandler) wsReader(chunkQueue chan<- []byte) error {
	for {
		messageType, data, err := h.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway,
				websocket.CloseAbnormalClosure,
				websocket.CloseNormalClosure) {
				h.logger.Printf("WebSocket read error: %v", err)
			}
			return err
		}

		if messageType != websocket.BinaryMessage {
			continue
		}

		// Non-blocking send so we don't stall the WebSocket reader if the
		// queue is momentarily full.
		select {
		case chunkQueue <- data:
		default:
			h.logger.Printf("Chunk queue full, dropping frame")
		}
	}
}

// pipeWriter drains chunkQueue and writes to the given pipe. It returns when
// the stop channel is closed or the pipe is closed.
func (h *StreamHandler) pipeWriter(w io.WriteCloser, chunkQueue <-chan []byte, stop <-chan struct{}) error {
	defer w.Close()

	for {
		select {
		case <-stop:
			return nil
		case chunk, ok := <-chunkQueue:
			if !ok {
				return nil
			}
			if _, err := w.Write(chunk); err != nil {
				// Any write error means the pipe is broken (ffmpeg exited).
				return nil
			}
		}
	}
}

// proxyFfmpegOutputToWebSocket reads ffmpeg stdout and sends chunks back
// over the WebSocket. This enables low-latency browser preview via MSE.
func (h *StreamHandler) proxyFfmpegOutputToWebSocket(r io.ReadCloser) error {
	defer r.Close()

	buf := make([]byte, 65536)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if err := h.conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				h.logger.Printf("WebSocket write error (preview): %v", err)
				return err
			}
		}
		if err != nil {
			if err != io.EOF {
				h.logger.Printf("ffmpeg stdout read error: %v", err)
				return err
			}
			break
		}
	}
	return nil
}

// extractVideoID extracts the video ID from a Cloudflare Stream HLS URL.
// Expected format: https://videodelivery.net/{id}/manifest/video.m3u8
func extractVideoID(videoURL string) string {
	const prefix = "https://videodelivery.net/"
	if !strings.HasPrefix(videoURL, prefix) {
		return ""
	}
	rest := videoURL[len(prefix):]
	if idx := strings.Index(rest, "/"); idx != -1 {
		return rest[:idx]
	}
	return rest
}
