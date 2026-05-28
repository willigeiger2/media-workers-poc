package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"media-workers-poc/ffmpeg"
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
// WebSocket messages to ffmpeg's stdin pipe.
func (h *StreamHandler) Run() error {
	streamURL := os.Getenv("STREAM_URL")
	if streamURL == "" {
		return fmt.Errorf("STREAM_URL environment variable not set")
	}

	// Step 1: Read configuration and first binary chunk.
	// The client may send an optional JSON text message first to select a
	// preset (e.g. overlay). If no text message is sent, we default to
	// passthrough.
	cfg, firstChunk, err := h.readConfigAndFirstChunk()
	if err != nil {
		return fmt.Errorf("failed to read stream config: %w", err)
	}

	h.logger.Printf("Config: preset=%q overlay=%q", cfg.Preset, cfg.OverlayImage)

	// Step 2: Create a pipe for ffmpeg input.
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to create pipe: %w", err)
	}
	defer pr.Close()

	// Step 3: Build ffmpeg arguments and start ffmpeg.
	args := ffmpeg.BuildArgs(cfg, "/dev/fd/3", streamURL)
	h.logger.Printf("Starting ffmpeg: %v", args)

	cmd := exec.Command("ffmpeg", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{pr}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ffmpeg: %w", err)
	}

	// Close the read end in the parent process — only ffmpeg should read it.
	pr.Close()

	// Step 4: Write the first chunk to the pipe.
	if _, err := pw.Write(firstChunk); err != nil {
		return fmt.Errorf("failed to write first chunk: %w", err)
	}
	h.logger.Printf("Wrote first chunk to ffmpeg pipe")

	// Step 5: Continue proxying remaining WebSocket messages to the pipe.
	wsDone := make(chan error, 1)
	go func() {
		wsDone <- h.proxyWebSocketToPipe(pw)
	}()

	// Step 6: Wait for ffmpeg exit.
	ffmpegDone := make(chan error, 1)
	go func() {
		ffmpegDone <- cmd.Wait()
	}()

	// Wait for either side to finish.
	select {
	case err := <-wsDone:
		if err != nil {
			h.logger.Printf("WebSocket proxy error: %v", err)
		}
		// WebSocket closed (client disconnected). Close the pipe to signal
		// EOF to ffmpeg, then wait for it to exit gracefully.
		h.logger.Printf("WebSocket closed, signaling EOF to ffmpeg")
		pw.Close()

		select {
		case err := <-ffmpegDone:
			if err != nil {
				h.logger.Printf("ffmpeg exited with error: %v", err)
				return fmt.Errorf("ffmpeg failed: %w", err)
			}
			h.logger.Printf("ffmpeg exited cleanly")
		case <-time.After(ffmpeg.ShutdownTimeout):
			h.logger.Printf("ffmpeg did not exit within %v, killing", ffmpeg.ShutdownTimeout)
			cmd.Process.Kill()
			<-ffmpegDone // reap
			return fmt.Errorf("ffmpeg shutdown timeout")
		}

	case err := <-ffmpegDone:
		// ffmpeg exited early (e.g., RTMP handshake failure).
		if err != nil {
			h.logger.Printf("ffmpeg exited early with error: %v", err)
			// Close the WebSocket so the client knows something went wrong.
			h.conn.Close()
			return fmt.Errorf("ffmpeg exited early: %w", err)
		}
		h.logger.Printf("ffmpeg exited cleanly (unexpectedly early)")
		h.conn.Close()
	}

	return nil
}

// readConfigAndFirstChunk reads messages from the WebSocket until it has
// both a Config (optional) and the first binary chunk (required).
// If no text message is received, it defaults to passthrough.
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
			continue // Keep reading for the first binary chunk.
		}

		if messageType == websocket.BinaryMessage {
			firstChunk = data
			break
		}

		// Ignore other message types (ping, pong, close).
	}

	return cfg, firstChunk, nil
}

// proxyWebSocketToPipe reads binary messages from the WebSocket and writes
// them to the given pipe. It returns when the WebSocket is closed or an
// error occurs.
func (h *StreamHandler) proxyWebSocketToPipe(w io.WriteCloser) error {
	defer w.Close()

	totalBytes := 0

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
			// Ignore non-binary messages (e.g., control messages).
			continue
		}

		totalBytes += len(data)

		if _, err := w.Write(data); err != nil {
			h.logger.Printf("Pipe write error: %v", err)
			return err
		}
	}
}
