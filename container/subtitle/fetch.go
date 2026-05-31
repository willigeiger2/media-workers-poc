// Package subtitle handles fetching, parsing, and converting WebVTT subtitle
// tracks from Cloudflare Stream videos for ffmpeg burn-in.
package subtitle

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// FetchResult contains the outcome of a subtitle fetch attempt.
type FetchResult struct {
	SubtitleFile string // Path to converted SRT file, empty if not found
	Language     string // Detected language code
	CueCount     int    // Number of subtitle cues
	Warning      string // User-facing warning if no subtitles found
}

// FetchAndConvert downloads the WebVTT subtitle track for a Stream video,
// converts it to SRT format, and returns the path to the SRT file.
//
// It tries the English track first, then falls back to the first available
// track. If no subtitles exist, returns a result with Warning set.
func FetchAndConvert(videoID string) (*FetchResult, error) {
	// Try English VTT first.
	vttURL := fmt.Sprintf("https://videodelivery.net/%s/captions/en.vtt", videoID)
	vttData, err := fetchURL(vttURL)
	if err != nil {
		// English not found — try to discover available tracks via manifest.
		vttURL, err = discoverSubtitleTrack(videoID)
		if err != nil {
			return &FetchResult{
				Warning: "No subtitles found for this video. Streaming without burn-in.",
			}, nil
		}
		vttData, err = fetchURL(vttURL)
		if err != nil {
			return &FetchResult{
				Warning: "No subtitles found for this video. Streaming without burn-in.",
			}, nil
		}
	}

	// Parse WebVTT to count cues and validate.
	cues, err := ParseWebVTT(string(vttData))
	if err != nil {
		return nil, fmt.Errorf("failed to parse WebVTT: %w", err)
	}

	// Convert to SRT.
	srtPath := filepath.Join(os.TempDir(), fmt.Sprintf("subtitle-%s.srt", videoID))
	srtData := WebVTTToSRT(cues)
	if err := os.WriteFile(srtPath, []byte(srtData), 0644); err != nil {
		return nil, fmt.Errorf("failed to write SRT file: %w", err)
	}

	// Extract language from URL.
	lang := "en"
	if idx := strings.LastIndex(vttURL, "/captions/"); idx != -1 {
		rest := vttURL[idx+len("/captions/"):]
		if dot := strings.Index(rest, "."); dot != -1 {
			lang = rest[:dot]
		}
	}

	return &FetchResult{
		SubtitleFile: srtPath,
		Language:     lang,
		CueCount:     len(cues),
	}, nil
}

// fetchURL performs an HTTP GET and returns the response body.
func fetchURL(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// discoverSubtitleTrack attempts to find a subtitle track by inspecting the
// HLS manifest for #EXT-X-MEDIA:TYPE=SUBTITLES entries.
func discoverSubtitleTrack(videoID string) (string, error) {
	manifestURL := fmt.Sprintf("https://videodelivery.net/%s/manifest/video.m3u8", videoID)
	data, err := fetchURL(manifestURL)
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			continue
		}
		if !strings.Contains(line, "TYPE=SUBTITLES") {
			continue
		}

		// Extract URI from the media tag.
		// Format: #EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",URI="captions/en.m3u8"
		uriStart := strings.Index(line, `URI="`)
		if uriStart == -1 {
			continue
		}
		uriStart += len(`URI="`)
		uriEnd := strings.Index(line[uriStart:], `"`)
		if uriEnd == -1 {
			continue
		}
		uri := line[uriStart : uriStart+uriEnd]

		// Resolve relative URI.
		if strings.HasPrefix(uri, "http") {
			return uri, nil
		}
		return fmt.Sprintf("https://videodelivery.net/%s/%s", videoID, uri), nil
	}

	return "", fmt.Errorf("no subtitle tracks found in manifest")
}
