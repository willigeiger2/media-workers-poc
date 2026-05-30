// Package overlay generates real-time overlay frames using a 2D graphics API.
// Frames are rendered with github.com/fogleman/gg and written as raw RGBA
// to a pipe consumed by ffmpeg.
package overlay

import (
	"fmt"
	"image"
	"image/color"
	"io"
	"log"
	"os"
	"time"

	"github.com/fogleman/gg"
)

// FrameGenerator renders text/graphics frames and writes raw RGBA to a pipe.
type FrameGenerator struct {
	Width            int
	Height           int
	Text             string
	FontPath         string
	FontSize         float64
	TextColor        color.Color
	BgColor          color.Color
	PositionX        float64
	PositionY        float64
	FrameRate        int
	TimezoneOffsetMin int // Offset from UTC in minutes (e.g., -420 for PDT)
	logger           *log.Logger
}

// NewFrameGenerator creates a generator with sensible defaults.
func NewFrameGenerator() *FrameGenerator {
	return &FrameGenerator{
		Width:             1280,
		Height:            720,
		Text:              "00:00:00",
		FontPath:          "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
		FontSize:          36,
		TextColor:         color.White,
		BgColor:           color.Transparent,
		PositionX:         20,
		PositionY:         50,
		FrameRate:         30,
		TimezoneOffsetMin: 0,
		logger:            log.New(os.Stderr, "[overlay] ", log.LstdFlags|log.Lmsgprefix),
	}
}

// Run blocks, rendering frames at FrameRate until the pipe is closed or
// the done channel is signaled.
func (g *FrameGenerator) Run(w io.WriteCloser, done <-chan struct{}) error {
	defer w.Close()

	// Try a fallback font path if the primary doesn't exist.
	fontPaths := []string{
		g.FontPath,
		"/usr/share/fonts/TTF/DejaVuSansMono.ttf",
		"/usr/share/fonts/liberation/LiberationMono-Regular.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationMono-Regular.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
	}

	var dc *gg.Context
	for _, fp := range fontPaths {
		ctx := gg.NewContext(g.Width, g.Height)
		if err := ctx.LoadFontFace(fp, g.FontSize); err == nil {
			dc = ctx
			g.logger.Printf("Loaded font: %s", fp)
			break
		}
	}
	if dc == nil {
		return fmt.Errorf("failed to load any font")
	}

	interval := time.Second / time.Duration(g.FrameRate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Pre-allocate the RGBA image backing store.
	rgbaImg := dc.Image().(*image.RGBA)

	// Background box styling.
	bgPadding := 10.0
	bgCornerRadius := 4.0
	bgColor := color.RGBA{0, 0, 0, 128} // 50% opacity black

	tzOffset := time.Duration(g.TimezoneOffsetMin) * time.Minute

	for {
		select {
		case <-done:
			g.logger.Printf("Frame generator stopped")
			return nil
		case <-ticker.C:
			// Clear to transparent background.
			dc.SetColor(g.BgColor)
			dc.Clear()

			// Compute local time.
			now := time.Now().UTC().Add(tzOffset)
			text := now.Format("15:04:05.000")

			// Measure text for background box.
			textW, textH := dc.MeasureString(text)
			boxX := g.PositionX - bgPadding
			boxY := g.PositionY - textH - bgPadding
			boxW := textW + bgPadding*2
			boxH := textH + bgPadding*2

			// Draw semi-transparent background box.
			dc.SetColor(bgColor)
			dc.DrawRoundedRectangle(boxX, boxY, boxW, boxH, bgCornerRadius)
			dc.Fill()

			// Draw the text.
			dc.SetColor(g.TextColor)
			dc.DrawString(text, g.PositionX, g.PositionY)

			// Write raw RGBA pixels to the pipe.
			if _, err := w.Write(rgbaImg.Pix); err != nil {
				if err == io.ErrClosedPipe || err == io.EOF {
					g.logger.Printf("Overlay pipe closed")
					return nil
				}
				return fmt.Errorf("overlay pipe write error: %w", err)
			}
		}
	}
}
