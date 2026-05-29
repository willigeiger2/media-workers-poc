package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gorilla/websocket"
)

func main() {
	port := flag.String("port", "8080", "HTTP server port")
	flag.Parse()

	server := NewServer()

	addr := fmt.Sprintf(":%s", *port)
	log.Printf("[server] Starting on %s", addr)
	if err := http.ListenAndServe(addr, server); err != nil {
		log.Fatalf("[server] Failed to start: %v", err)
	}
}

// Server is the HTTP front-end for the media pass-through container.
type Server struct {
	upgrader websocket.Upgrader
}

// NewServer constructs a Server with all routes registered.
func NewServer() *Server {
	allowedOrigins := getAllowedOrigins()

	return &Server{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				// Allow empty origin (non-browser clients)
				if origin == "" {
					return true
				}
				// Check against allowlist
				for _, allowed := range allowedOrigins {
					if strings.HasPrefix(origin, allowed) {
						return true
					}
				}
				log.Printf("[server] Rejected WebSocket from origin: %s", origin)
				return false
			},
		},
	}
}

// getAllowedOrigins returns the list of allowed WebSocket origins.
// Defaults to localhost for development. Set ALLOWED_ORIGINS env var
// for production (comma-separated list).
func getAllowedOrigins() []string {
	if env := os.Getenv("ALLOWED_ORIGINS"); env != "" {
		return strings.Split(env, ",")
	}
	// Default: allow localhost on any port for development
	return []string{
		"http://localhost:",
		"https://localhost:",
		"http://127.0.0.1:",
		"https://127.0.0.1:",
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health":
		s.handleHealth(w, r)
	case "/ws":
		s.handleWebSocket(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[server] WebSocket upgrade failed: %v", err)
		return
	}

	log.Printf("[server] WebSocket connection established from %s", conn.RemoteAddr())

	// Spawn a handler for this connection. Each connection gets its own
	// ffmpeg process.
	handler := NewStreamHandler(conn)
	if err := handler.Run(); err != nil {
		log.Printf("[server] Stream handler error: %v", err)
	}

	log.Printf("[server] WebSocket connection closed from %s", conn.RemoteAddr())
}
