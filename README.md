# Media Workers PoC

This project demonstrates a video pass-through pipeline using Cloudflare Workers, WebSockets, and Containers with ffmpeg.

## What it does

1. **Browser** captures webcam video using `MediaRecorder`
2. **Browser** sends video chunks (WebM/VP8) over a WebSocket to the Worker
3. **Worker** (Astro + Cloudflare) proxies the WebSocket to the container
4. **Container** (Go) receives chunks and pipes them to ffmpeg
5. **ffmpeg** transcodes VP8 → H.264 and outputs RTMP
6. **RTMP** goes to a Cloudflare Stream Live Input for broadcast

### Features

- **Pass-through** (`/`): Webcam → WebSocket → ffmpeg → RTMP → Stream
- **Overlay demo** (`/overlay`): Same pipeline but composites a Cloudflare logo on top of the video using ffmpeg `filter_complex`
- **Low-latency preview** (Settings): Instead of sending to Stream Live, the container sends the processed video back over the WebSocket. The browser plays it via MediaSource Extensions (MSE) with ~300-500ms latency.

### Presets (Container-Side)

The container supports a JSON config message sent as the first text message on the WebSocket:

```json
{"preset": "overlay", "overlay_image": "/app/overlays/cf-logo.png", "overlay_position": "top-right"}
```

Supported presets:
- `passthrough` (default): Just transcode and send to RTMP
- `overlay`: Composite a static PNG image using ffmpeg `overlay` filter
- Positions: `top-right`, `top-left`, `bottom-right`, `bottom-left`

Additional config fields:
- `preview_mode` (bool): When `true`, ffmpeg outputs fragmented MP4 to stdout and the container streams it back over the WebSocket for browser playback.
- `stream_url` (string): Override the RTMP destination (ignored when `preview_mode` is `true`).

Escape hatch for experimentation:
```json
{"ffmpeg_args": ["-f", "webm", "-i", "/dev/fd/3", "..."]}
```

## Architecture

```
┌─────────────┐      WS        ┌─────────────┐      WS       ┌─────────────┐
│   Browser   │ ──────────────> │   Worker    │ ─────────────>│  Container  │
│  (webcam)   │  (WebM chunks)  │  (Astro/CF) │  (proxy)      │   (Go)      │
└─────────────┘                 └─────────────┘               └──────┬──────┘
                                                                     │
                                                                     v
                                                              ffmpeg ──> RTMP
                                                                          │
                                                                          v
                                                                   Stream Live
```

## Prerequisites

- **Node.js** >= 22.12.0
- **Go** >= 1.25
- **Docker** and **Docker Compose**
- A **Cloudflare Stream Live Input** with an RTMP URL

## Project Structure

```
.
├── container/           # Go application (runs in Docker locally)
│   ├── main.go         # HTTP server + WebSocket handler
│   ├── handler.go      # Stream handler (ffmpeg spawn + proxy)
│   ├── ffmpeg/
│   │   └── args.go     # ffmpeg argument builder
│   ├── media/
│   │   └── source.go   # MediaSource interface (for future WebRTC)
│   ├── Dockerfile
│   ├── go.mod
│   └── go.sum
├── src/
│   ├── pages/
│   │   ├── index.astro     # Webcam streaming page
│   │   ├── watch.astro     # Stream playback page
│   │   └── api/
│   │       └── stream.ts   # WebSocket proxy API route
│   └── layouts/
│       └── Layout.astro
├── docker-compose.yml
├── wrangler.jsonc
├── .env.example
└── README.md
```

## Setup

### 1. Configure environment variables

Copy `.env.example` to `.env` and fill in your Stream Live Input URLs:

```bash
cp .env.example .env
```

Edit `.env`:

```env
# Cloudflare Stream Live Input RTMP ingest URL
# 
# From the dashboard: Stream → Live Inputs → Your Input → Connection Info
# Shows URL + Key separately. Concatenate them for the full URL:
STREAM_URL=rtmps://live.cloudflare.com:443/live/YOUR_STREAM_KEY

# Cloudflare Stream playback URL (for the /watch page)
# 
# Use a WEB playback URL (iframe embed or HLS), NOT the RTMPS Playback Key.
# The RTMPS Playback Key is for pulling into OBS/VLC, not browser viewing.
#
# From the dashboard, use one of:
#   - iframe embed:  https://iframe.cloudflarestream.com/LIVE_INPUT_ID
#   - HLS manifest:  https://customer-xxx.cloudflarestream.com/LIVE_INPUT_ID/manifest/video.m3u8
STREAM_PLAYBACK_URL=https://iframe.cloudflarestream.com/YOUR_LIVE_INPUT_ID

# RTMP Playback URL (for low-latency preview via ffplay)
#
# From the dashboard: Stream → Live Inputs → Your Input → Connection Info
# This is the "RTMPS Playback Key" — different from the ingest STREAM_URL above.
# Used by the streaming page to show the exact ffplay command with a copy button.
RTMP_PLAYBACK_URL=rtmps://live.cloudflare.com:443/live/YOUR_PLAYBACK_KEY

# Container port (local dev)
CONTAINER_PORT=8788
```

### 2. Install dependencies

```bash
npm install
```

## Running Locally

You need to run **two separate services** in **two separate terminals**:
1. **The Container** (Go + ffmpeg) — handles video processing
2. **The Worker** (Astro + Cloudflare) — serves the web UI and proxies WebSocket

### Prerequisites

- [Docker](https://docs.docker.com/get-docker/) running locally
- (Optional) [Docker Compose](https://docs.docker.com/compose/install/) — if unavailable, use the shell script below
- Dependencies installed: `npm install`
- Astro project built: `npm run build`

---

### Terminal 1: Start the Container

**Note for Cloudflare employees:** If you're behind WARP, the Dockerfile copies the Cloudflare root CA certificate from `~/.local/share/cloudflare-warp-certs/` so Alpine can fetch packages. If you see SSL errors, make sure that path exists.

**Option A: Docker Compose**

```bash
docker-compose up --build
```

**Option B: Shell script (if Docker Compose is not installed)**

```bash
./run-container.sh
```

**Option C: Manual Docker**

```bash
docker build -t media-workers-poc ./container
docker run --rm -p 8788:8080 -e STREAM_URL=rtmp://... media-workers-poc
```

**Expected output:**
```
[server] Starting on :8080
```

Leave this terminal running. The container will be available on `localhost:8788`.

---

### Terminal 2: Start the Worker

**Important:** You must build the Astro project first, then run Wrangler pointing to the built entry file:

```bash
# Make sure you've built first
npm run build

# Clean stale deploy config (run this if you see a config conflict error)
rm -rf .wrangler/deploy

# Run the Worker
npx wrangler dev dist/server/entry.mjs
```

Or use the provided script which handles the cleanup automatically:

```bash
./run-worker.sh
```

**Expected output:**
```
⎔ Starting local server...
[wrangler:info] Ready on http://localhost:8787
```

Leave this terminal running too. The Worker serves:
- The Astro frontend at `http://localhost:8787/`
- The WebSocket proxy API at `ws://localhost:8787/api/stream`
- The watch page at `http://localhost:8787/watch`

### Step 3: Stream

1. Open the Worker URL in a browser (e.g., `http://localhost:8787`)
2. Click **Start Streaming**
3. Allow camera access
4. You should see the local preview and status updates

### Step 4: Watch

- Open `/watch` on the same host (e.g., `http://localhost:8787/watch`)
- Or go to the Cloudflare Stream dashboard to view your live input

## Testing the Container Directly

To verify the container works independently of the Worker:

```bash
# Build and run just the container
cd container
docker build -t media-poc .
docker run -p 8788:8080 -e STREAM_URL=rtmp://... media-poc
```

Then use the included test client or any WebSocket client to send WebM data.

## Deployment

### Deploying the Container (Production)

For production, the container would be deployed via Cloudflare Containers. This requires:

1. Building and pushing the image to a registry
2. Configuring the container binding in `wrangler.jsonc`

Example container binding (add to `wrangler.jsonc`):

```json
{
  "containers": {
    "MEDIA_CONTAINER": {
      "source": {
        "image": "registry.cfdata.org/your-registry/media-workers-poc:latest"
      },
      "resources": {
        "cpu": 2,
        "memory": "4GB"
      }
    }
  }
}
```

### Deploying the Worker

```bash
npm run deploy
```

This builds the Astro app and deploys it as a Cloudflare Worker.

### Setting Secrets

For production, set the Stream URL as a secret:

```bash
wrangler secret put STREAM_URL
```

## How It Works (Detailed)

### Browser → WebSocket

The browser uses `getUserMedia()` to capture the webcam and `MediaRecorder` to encode it as WebM/VP8. Every 100ms, a chunk is produced and sent as a binary WebSocket message to `/api/stream`.

### Worker → Container

The Astro API route at `/api/stream` handles the WebSocket upgrade. It creates a `WebSocketPair`, accepts the server side, and opens a second WebSocket connection to the container. Messages are proxied bidirectionally.

### Container → ffmpeg → RTMP

The Go container runs an HTTP server with a `/ws` endpoint. When a WebSocket connects:

1. A pipe is created (`os.Pipe()`)
2. ffmpeg is spawned with the read end of the pipe as input (`/dev/fd/3`)
3. ffmpeg is configured to:
   - Read WebM/VP8 from the pipe
   - Transcode to H.264 with `libx264`
   - Output FLV format for RTMP
4. A goroutine reads WebSocket messages and writes them to the pipe
5. When the WebSocket closes, the pipe is closed, ffmpeg sees EOF, finalizes the output, and exits

### Low-Latency Browser Preview (Preview Mode)

When **Enable low-latency browser preview** is turned on in Settings, the pipeline changes:

1. The browser sends `preview_mode: true` in the JSON config message
2. ffmpeg outputs **fragmented MP4** (`-f mp4 -movflags frag_keyframe+empty_moov+default_base_moof`) to stdout instead of RTMP
3. A second goroutine in the container reads ffmpeg stdout and sends binary chunks back over the WebSocket
4. The browser receives the chunks and plays them via **MediaSource Extensions** in a `<video>` element
5. Latency is ~300-500ms (just the encoding + network hop)

This mode works with both the `passthrough` and `overlay` presets.

### Graceful Shutdown

If the client disconnects:
1. The WebSocket read loop exits
2. The pipe write end is closed
3. ffmpeg receives EOF and can finalize the FLV/RTMP stream
4. We wait up to 10 seconds for ffmpeg to exit cleanly
5. If ffmpeg hangs, it is killed

This pattern is borrowed from the production `webrtc-recording-worker` (VCR) project.

## Future Stages

- **Stage 1b**: Add audio pass-through
- **Stage 2** (partial): ✅ Static image overlay with `filter_complex` — dynamic overlay and multi-input compositor still planned
- **Stage 3**: Analyzer — extract frames, store in Images, generate descriptions
- **Stage X**: Replace WebSocket ingress with WebRTC/WHIP

## Troubleshooting

### "Cannot read RTMP handshake response"

Check that your `STREAM_URL` is correct and the Stream Live Input is active.

### Container fails to start

Check Docker logs:
```bash
docker-compose logs -f container
```

### WebSocket connection fails

Make sure the container is running on port 8788 before starting the Worker.

### ffmpeg uses 100% CPU

Software H.264 encoding (`libx264`) is CPU-intensive. This is expected without GPU acceleration. For production, use hardware encoding (`h264_nvenc` on NVIDIA GPUs).

## Credits

- ffmpeg pipe handling and graceful shutdown inspired by [`cloudflare/vid/webrtc-recording-worker`](https://gitlab.cfdata.org/cloudflare/vid/webrtc-recording-worker)
