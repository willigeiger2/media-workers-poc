# Media Workers PoC — Stages & Next Steps

This document tracks what's been implemented and what's planned for future sessions.

## Completed: Stage 1 — WebSocket Pass-Through

**Status:** ✅ Complete and tested end-to-end

**Goal:** Get webcam video from browser → WebSocket → Worker → Container → ffmpeg → RTMP → Stream Live

### What's working
- [x] Go container with HTTP server (`/health`, `/ws`)
- [x] WebSocket handler that spawns ffmpeg per connection
- [x] ffmpeg args: WebM/VP8 → H.264 → FLV → RTMP
- [x] Graceful shutdown (close pipe → ffmpeg sees EOF → finalizes output)
- [x] Docker + Docker Compose setup
- [x] Astro frontend with webcam capture (`getUserMedia` + `MediaRecorder`)
- [x] WebSocket proxy API route (`/api/stream`)
- [x] Playback page with Cloudflare Stream embed (`/watch`)
- [x] Pluggable `MediaSource` interface (ready for WebRTC)
- [x] ffplay command with copy-to-clipboard button on streamer page
- [x] `.env.example` for template-based coworking (values copied manually)

### Session log

**2025-05-26:** Initial build + testing session
- Pipeline verified working: webcam → WebSocket → Worker → Container → ffmpeg → RTMP → Stream
- Debug logging cleaned up (removed per-message logs from Worker proxy and container)
- Terminology standardized: "video ID" → "Live Input ID" across codebase
- `RTMP_PLAYBACK_URL` support added with copy-to-clipboard button
- `byteLength` check used instead of `instanceof ArrayBuffer` for Workers cross-realm compatibility
- `.env.example` created for template-based coworking

### Not yet done
- [ ] **Deployment** — Worker + container have not been deployed to Cloudflare yet. `wrangler.jsonc` container binding is TBD.

### Architecture
```
Browser (webcam) → MediaRecorder (WebM/VP8)
  ↓
WebSocket (/api/stream)
  ↓
Worker (Astro + Cloudflare) — proxies WS to container
  ↓
WebSocket (ws://localhost:8788/ws)
  ↓
Go Container — os.Pipe() → ffmpeg stdin
  ↓
ffmpeg — libx264 transcode → FLV → RTMP
  ↓
Cloudflare Stream Live Input
```

### Files created
```
container/
  main.go              # HTTP server
  handler.go           # WS → pipe → ffmpeg proxy
  ffmpeg/args.go       # ffmpeg arg builder
  media/source.go      # MediaSource interface
  Dockerfile           # Multi-stage build
  go.mod / go.sum      # Go dependencies
src/
  pages/
    index.astro        # Streamer page
    watch.astro        # Playback page
    api/stream.ts      # WS proxy API route
  layouts/Layout.astro # Updated with title prop
docker-compose.yml     # Container orchestration
run-container.sh       # Alternative to docker-compose
run-worker.sh          # Build + start Worker with cleanup
wrangler.jsonc         # Worker config (container binding TBD)
.env.example           # Env vars documented (template for coworking)
README.md              # Full setup guide
STAGES.md              # This file — roadmap and session log
```

## Stage 1b — Audio Pass-Through (Next)

**Goal:** Add audio to the pass-through pipeline

### What to do
- [ ] Update `MediaRecorder` to capture audio: `{ video: true, audio: true }`
- [ ] Update ffmpeg args to accept audio stream (Opus in WebM)
- [ ] Transcode Opus → AAC for RTMP/FLV compatibility
- [ ] Handle A/V sync (ffmpeg should manage this, but verify)
- [ ] Test with real Stream Live Input

### Files to change
- `src/pages/index.astro` — add audio constraints to `getUserMedia`
- `container/ffmpeg/args.go` — add audio input and codec args
- `container/handler.go` — create second pipe for audio (or interleaved WebM)

**Open question:** Does MediaRecorder produce interleaved A/V WebM, or do we need separate tracks?

## Stage 2 — Compositor (Overlay Demo)

**Status:** ✅ Static image overlay working (2025-05-27)

**Goal:** Accept two video streams and composite them (e.g., webcam + overlay)

### What's working
- [x] WebSocket protocol supports JSON config text message before binary stream
- [x] ffmpeg `filter_complex` with `overlay` filter composites PNG on live video
- [x] Position presets: top-right (default), top-left, bottom-right, bottom-left
- [x] Escape hatch: raw `ffmpeg_args` for experimentation
- [x] `/overlay` page demonstrates the feature

### What's planned
- [ ] Multiple video inputs (two live streams, not just image + video)
- [ ] Dynamic overlay (HTML5 Canvas-rendered frames sent as second input)
- [ ] Server-side overlay rendering in Worker (no Canvas API — needs alternative)

### Architecture
```
Browser (overlay page)
  → WS text: {"preset":"overlay","overlay_image":"..."}
  → WS binary: WebM chunks
  → Worker proxy
  → Container
    → Parses JSON config
    → Runs ffmpeg with filter_complex:
      [1:v]scale=200:-1[logo];[0:v][logo]overlay=W-w-10:10
    → RTMP to Stream
```

### Files changed
- `container/ffmpeg/args.go` — `BuildArgs()` preset registry, `BuildOverlayArgs()`
- `container/handler.go` — reads JSON config before binary stream
- `container/Dockerfile` — copies overlay assets into image
- `src/pages/overlay.astro` — overlay demo page
- `src/pages/index.astro` — link to overlay demo

## Stage 3 — Analyzer (Frames + Descriptions)

**Goal:** Extract still frames from video, store in Cloudflare Images, generate descriptions

### What to do
- [ ] Worker fetches VOD from Cloudflare Stream (m3u8 manifest)
- [ ] Worker sends segments to Analyzer container via WebSocket
- [ ] ffmpeg extracts frames (e.g., 1fps)
- [ ] Upload frames to Cloudflare Images API
- [ ] Send frames to Workers AI (LLM) for description generation
- [ ] Output JSON with image URLs + descriptions
- [ ] Worker renders web page from JSON

### Architecture
```
Worker (m3u8 fetcher) → segments → Container (ffmpeg)
  ↓
ffmpeg extracts frames → Cloudflare Images
  ↓
Workers AI (LLM) → descriptions
  ↓
JSON → Worker → HTML page
```

### Files to create
- `src/pages/analyzer.astro` — VOD input + results page
- New container mode or separate binary for analysis
- `container/analyzer/` or extend handler with modes

## Stage X — WebRTC/WHIP Input

**Goal:** Replace WebSocket ingress with WebRTC (lower latency, better congestion control)

### What to do
- [ ] Implement WHIP client in browser (or use Stream's WHIP endpoint)
- [ ] Implement WHEP ingestion in container (borrow from VCR project)
- [ ] RTP demuxing → ffmpeg pipes
- [ ] Signaling server (could be a Worker or the container itself)

### Reference
- [`cloudflare/vid/webrtc-recording-worker`](https://gitlab.cfdata.org/cloudflare/vid/webrtc-recording-worker) — production VCR implementation
- VCR uses `pion/webrtc` for WebRTC, WHEP client, RTP → pipe → ffmpeg

## Open Questions / Decisions

1. **Container DO sidecar:** VCR uses a Durable Object (`VcrContainerSidecar`) to manage container lifecycle. Should we adopt this pattern?
   - Pros: Handles container eviction, restart, state management
   - Cons: More complexity, not needed for PoC
   - **Decision:** Skip for now, add when moving to production

2. **Audio in WebM:** Does `MediaRecorder` with `{ audio: true }` produce a single interleaved WebM file, or separate tracks?
   - **Research needed:** Test with `ffmpeg -i` on captured blobs

3. **Overlay rendering in Workers:** HTML5 Canvas is not available in Workers. Options:
   - Render in browser, send overlay frames as second WS stream
   - Use `@cloudflare/puppeteer` (Browser Rendering) for server-side rendering
   - Use a second container with Node.js + `skia-canvas`
   - **Decision:** Start with browser-rendered overlay for simplicity

4. **GPU acceleration:** Software H.264 encoding (`libx264`) is CPU-intensive. When do we add hardware encoding?
   - **Decision:** Not needed for PoC, but monitor performance

## How to pick up next session

1. Check this file for the next unchecked item
2. Review `README.md` for build/run instructions
3. Make sure Docker is running and `.env` is configured
4. Start container: `docker-compose up` or `./run-container.sh`
5. Build and start Worker: `npm run build && ./run-worker.sh`
6. Open browser to the Wrangler dev URL

## Useful commands

```bash
# Build container
docker build -t media-workers-poc ./container

# Run container
docker run --rm -p 8788:8080 -e STREAM_URL=rtmp://... media-workers-poc

# Run with compose
docker-compose up --build

# Start Worker
npx wrangler dev

# Deploy Worker
npm run deploy
```
