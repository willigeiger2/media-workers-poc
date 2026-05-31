// Package hlsdownloader downloads HLS VOD segments and feeds them as a
// continuous byte stream to an io.WriteCloser.
package hlsdownloader

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxRetries    = 3
	retryDelay    = 500 * time.Millisecond
	readAheadSegs = 3 // number of segments to pre-fetch
)

// Segment represents a single HLS segment with its URL and duration.
type Segment struct {
	URL      string
	Duration float64
}

// VariantInfo holds the parsed variant manifest data.
type VariantInfo struct {
	Segments  []Segment
	InitURL   string // fMP4 init segment (from #EXT-X-MAP), empty for TS
	IsFMP4    bool   // true if segments are .m4s (fMP4), false if .ts
	BaseURL   string
}

// SegmentData holds downloaded segment bytes plus its declared duration.
type SegmentData struct {
	Data     []byte
	Duration float64
}

// DownloadAndStream fetches the HLS manifest at manifestURL, downloads all
// video segments in order, and writes them continuously to w.  When the VOD
// playlist is exhausted w is closed to signal EOF.
func DownloadAndStream(manifestURL string, w io.WriteCloser) error {
	defer w.Close()

	client := &http.Client{Timeout: 30 * time.Second}

	// 1. Fetch master manifest and pick the best video variant.
	log.Printf("[hlsdownloader] Fetching master manifest: %s", manifestURL)
	variantURL, err := pickVariant(client, manifestURL)
	if err != nil {
		return fmt.Errorf("pick variant: %w", err)
	}
	log.Printf("[hlsdownloader] Selected variant: %s", variantURL)

	// 2. Fetch variant manifest and parse segment list + init segment.
	log.Printf("[hlsdownloader] Fetching variant manifest: %s", variantURL)
	info, err := parseVariantManifest(client, variantURL)
	if err != nil {
		return fmt.Errorf("parse variant manifest: %w", err)
	}
	if len(info.Segments) == 0 {
		return fmt.Errorf("no segments found in variant manifest")
	}
	log.Printf("[hlsdownloader] Found %d segments (fMP4=%v, init=%s)",
		len(info.Segments), info.IsFMP4, info.InitURL)

	// 3. For fMP4, write the init segment first.
	if info.IsFMP4 && info.InitURL != "" {
		log.Printf("[hlsdownloader] Downloading init segment: %s", info.InitURL)
		initData, err := fetchWithRetry(client, info.InitURL)
		if err != nil {
			return fmt.Errorf("init segment: %w", err)
		}
		log.Printf("[hlsdownloader] Writing init segment (%d bytes)", len(initData))
		if _, err := w.Write(initData); err != nil {
			log.Printf("[hlsdownloader] Pipe write error on init: %v", err)
			return nil
		}
	}

	// 4. Pre-fetch segments into a buffered channel (keeps 2 ahead).
	segCh := make(chan SegmentData, 2)
	errCh := make(chan error, 1)
	go func() {
		errCh <- prefetchSegments(client, info.Segments, segCh)
	}()

	// 5. Drain the channel and write to the pipe at real-time pace.
	segCount := 0
	totalBytes := 0
	for seg := range segCh {
		segCount++
		totalBytes += len(seg.Data)
		log.Printf("[hlsdownloader] Writing segment %d (%d bytes) to pipe", segCount, len(seg.Data))

		start := time.Now()
		if _, err := w.Write(seg.Data); err != nil {
			// Pipe broken — ffmpeg likely exited.
			log.Printf("[hlsdownloader] Pipe write error (ffmpeg exited?): %v", err)
			return nil
		}
		elapsed := time.Since(start)

		// Pace writing to maintain real-time playback.  If the pipe
		// backpressure already slowed us down (ffmpeg reading at 1x),
		// elapsed will be close to seg.Duration and we won't sleep.
		sleepTime := time.Duration(seg.Duration*float64(time.Second)) - elapsed
		if sleepTime > 0 {
			log.Printf("[hlsdownloader] Pacing: sleeping %v after segment %d", sleepTime, segCount)
			time.Sleep(sleepTime)
		}
	}

	// Wait for prefetch goroutine to finish.
	if err := <-errCh; err != nil {
		return err
	}

	log.Printf("[hlsdownloader] Finished: wrote %d segments, %d total bytes", segCount, totalBytes)
	return nil
}

// pickVariant downloads the master manifest and returns the URL of the
// highest-bandwidth video variant.
func pickVariant(client *http.Client, masterURL string) (string, error) {
	data, err := fetchWithRetry(client, masterURL)
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")
	var bestBandwidth int
	var bestURL string
	baseURL := baseOf(masterURL)

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}
		// Parse BANDWIDTH attribute.
		bw := parseBandwidth(line)
		if bw > bestBandwidth && i+1 < len(lines) {
			bestBandwidth = bw
			bestURL = resolveURL(baseURL, strings.TrimSpace(lines[i+1]))
		}
	}

	if bestURL == "" {
		// No variants found — master may be a single media playlist.
		return masterURL, nil
	}
	return bestURL, nil
}

// parseVariantManifest downloads a media playlist and returns the segment list
// and init segment (for fMP4).
func parseVariantManifest(client *http.Client, variantURL string) (*VariantInfo, error) {
	data, err := fetchWithRetry(client, variantURL)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(data), "\n")
	info := &VariantInfo{
		BaseURL: baseOf(variantURL),
	}
	var pendingDuration float64

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Parse init segment for fMP4 playlists.
		if strings.HasPrefix(line, "#EXT-X-MAP:") {
			info.InitURL = parseMapURI(line, info.BaseURL)
			info.IsFMP4 = true
			continue
		}

		if strings.HasPrefix(line, "#EXTINF:") {
			pendingDuration = parseDuration(line)
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Non-comment, non-empty line is a segment URI.
		segURL := resolveURL(info.BaseURL, line)
		info.Segments = append(info.Segments, Segment{
			URL:      segURL,
			Duration: pendingDuration,
		})
		if strings.HasSuffix(segURL, ".m4s") {
			info.IsFMP4 = true
		}
		pendingDuration = 0
	}

	return info, nil
}

// prefetchSegments downloads segments in order and pushes them into the
// channel.  The channel is closed when all segments are sent or an error
// occurs.
func prefetchSegments(client *http.Client, segments []Segment, out chan<- SegmentData) error {
	defer close(out)

	for _, seg := range segments {
		data, err := fetchWithRetry(client, seg.URL)
		if err != nil {
			return fmt.Errorf("segment %s: %w", seg.URL, err)
		}
		out <- SegmentData{Data: data, Duration: seg.Duration}
	}
	return nil
}

// parseMapURI extracts the URI from an #EXT-X-MAP tag and resolves it.
// Format: #EXT-X-MAP:URI="init.mp4"
func parseMapURI(line, baseURL string) string {
	uriStart := strings.Index(line, `URI="`)
	if uriStart == -1 {
		return ""
	}
	uriStart += len(`URI="`)
	uriEnd := strings.Index(line[uriStart:], `"`)
	if uriEnd == -1 {
		return ""
	}
	uri := line[uriStart : uriStart+uriEnd]
	return resolveURL(baseURL, uri)
}

// fetchWithRetry performs an HTTP GET with retries on failure.
func fetchWithRetry(client *http.Client, url string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(retryDelay)
		}
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && err == nil {
			return body, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}
	return nil, fmt.Errorf("failed after %d attempts: %w", maxRetries, lastErr)
}

// parseBandwidth extracts the BANDWIDTH value from an EXT-X-STREAM-INF line.
func parseBandwidth(line string) int {
	idx := strings.Index(line, "BANDWIDTH=")
	if idx == -1 {
		return 0
	}
	rest := line[idx+len("BANDWIDTH="):]
	if end := strings.IndexAny(rest, ",\""); end != -1 {
		rest = rest[:end]
	}
	bw, _ := strconv.Atoi(rest)
	return bw
}

// parseDuration extracts the duration from an EXTINF line.
func parseDuration(line string) float64 {
	// Format: #EXTINF:10.000,
	rest := line[len("#EXTINF:"):]
	if comma := strings.Index(rest, ","); comma != -1 {
		rest = rest[:comma]
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(rest), 64)
	return d
}

// baseOf returns the base URL (scheme+host+path dir) of a URL.
func baseOf(url string) string {
	if idx := strings.LastIndex(url, "/"); idx != -1 {
		return url[:idx+1]
	}
	return url + "/"
}

// resolveURL resolves a potentially relative URI against a base URL.
func resolveURL(base, uri string) string {
	if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {
		return uri
	}
	if strings.HasPrefix(uri, "/") {
		// Absolute path — need host from base.
		if schemeEnd := strings.Index(base, "://"); schemeEnd != -1 {
			scheme := base[:schemeEnd+3]
			rest := base[schemeEnd+3:]
			if hostEnd := strings.Index(rest, "/"); hostEnd != -1 {
				return scheme + rest[:hostEnd] + uri
			}
		}
	}
	// Relative path.
	return base + uri
}
