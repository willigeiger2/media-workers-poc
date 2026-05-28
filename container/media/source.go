// Package media defines the input source abstraction for media streams.
//
// In Stage 1 we only have a WebSocket source. In future stages this
// interface will also be implemented by a WHEP/WebRTC source.
package media

import "io"

// Source is a readable media stream. It is intentionally a thin wrapper
// around io.ReadCloser so that ffmpeg can consume it directly via os.Pipe.
type Source interface {
	io.Reader
	io.Closer
}
