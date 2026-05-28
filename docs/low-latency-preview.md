# Low-Latency Browser Preview (Option 2)

Raw H.264 over the existing WebSocket, decoded in the browser with WebCodecs.

**Target latency:** ~1s (vs 10-15s for HLS)
**Trade-off:** Chrome/Edge only, more CPU in browser

---

## Architecture

The existing WebSocket is already full-duplex. We just start sending data back:

```
Browser                          Container (Go)
   |                                    |
   |──binary WebM chunks──►             |
   |  (MediaRecorder)                   |
   |                                    ├───► ffmpeg #1
   |                                    |     WebM → H.264 (Annex-B)
   |                                    |     pipe:
   |                                    │         ├───► ffmpeg #2
   |                                    │         │     H.264 → FLV → RTMP
   |                                    │         │     (existing Stream output)
   |                                    │         │
   |◄────────binary H.264 NALs──────────┘         │
   |   (WebCodecs decode)                           │
   |                                                │
```

**Key insight:** The Worker proxy already forwards messages from container → browser (`containerWs.addEventListener("message", ...)`). We just need the container to *send* them.

---

## Container Changes

### New: second ffmpeg process

`container/preview.go` (new file):

```go
package main

import (
    "io"
    "os/exec"
)

// startPreviewEncoder transcodes the WebM input to raw H.264 Annex-B
// and returns a ReadCloser that yields H.264 NAL units.
func startPreviewEncoder(input io.Reader) (io.ReadCloser, error) {
    cmd := exec.Command("ffmpeg",
        "-i", "pipe:0",               // WebM from browser
        "-c:v", "libx264",
        "-preset", "ultrafast",
        "-tune", "zerolatency",
        "-g", "30",                   // keyframe every 30 frames (~1s)
        "-profile:v", "baseline",
        "-level", "3.0",
        "-bsf:v", "h264_mp4toannexb", // ensure Annex-B start codes
        "-f", "h264",                 // raw H.264 NAL stream
        "pipe:1",
    )

    cmd.Stdin = input
    stdout, err := cmd.StdoutPipe()
    if err != nil {
        return nil, err
    }

    if err := cmd.Start(); err != nil {
        return nil, err
    }

    // Return stdout; when closed, cmd will be killed
    return &killOnClose{ReadCloser: stdout, cmd: cmd}, nil
}

type killOnClose struct {
    io.ReadCloser
    cmd *exec.Cmd
}

func (k *killOnClose) Close() error {
    k.cmd.Process.Kill()
    return k.ReadCloser.Close()
}
```

### Modify handler to tee the stream

In `container/handler.go`, replace the single `os.Pipe()` with a `io.TeeReader` that feeds both the RTMP ffmpeg and the preview encoder:

```go
// Before (current):
// pr, pw := os.Pipe()
// go proxyWebSocketToPipe(pw)        // browser → pipe → ffmpeg (RTMP)

// After:
pr, pw := os.Pipe()

// Tee: everything from WebSocket goes to both RTMP and preview
previewReader := io.TeeReader(pr, previewWriter) // pseudo-code, see below
```

Actually, a cleaner approach: **two independent ffmpeg processes reading from the same WebSocket source.** Since the source is VP8/WebM, you can't easily tee the encoded stream — you need to decode first. So:

```go
// One goroutine reads WebSocket and writes to a buffer/queue
// Two consumers read from that queue:
//   1. existing RTMP ffmpeg
//   2. new preview ffmpeg
```

Or simpler: **the browser already sends the same chunks to the container.** We can have the container spawn TWO separate pipelines, each reading from the same WebSocket. But `gorilla/websocket` doesn't support multiple readers.

**Simplest correct approach:**
```go
pr, pw := os.Pipe()

// Goroutine 1: WebSocket → pipe (same as now)
go proxyWebSocketToPipe(pw)

// Goroutine 2: pipe → RTMP ffmpeg (same as now)
go runRTMPFFmpeg(pr)

// Goroutine 3: pipe → Preview ffmpeg
// We need another copy of the stream, so use io.TeeReader:
pr2, pw2 := os.Pipe()
tee := io.TeeReader(pr, pw2)

go runRTMPFFmpeg(tee)      // reads from tee, also copies to pw2
go runPreviewFFmpeg(pr2)   // reads the copy
```

Wait, `io.TeeReader` reads from `pr` and writes a copy to `pw2`. So `runRTMPFFmpeg(tee)` will read from `pr` and also write to `pw2`. Then `runPreviewFFmpeg(pr2)` reads from `pr2` (the read end of `pw2`'s pipe). This works!

### Send H.264 back to browser

In `container/handler.go`, after starting the preview encoder, read NAL units and send them:

```go
// Read Annex-B NAL units from preview ffmpeg and send to WebSocket
// H.264 Annex-B format: [00 00 00 01 <nal_unit>]*
// We can send individual NAL units or groups of them as single WS messages.
buf := make([]byte, 64*1024) // 64KB max NAL unit
for {
    n, err := previewOut.Read(buf)
    if err != nil {
        return
    }
    h.conn.WriteMessage(websocket.BinaryMessage, buf[:n])
}
```

For cleaner framing, you might want to parse Annex-B start codes and send one **Access Unit** (one frame) per WebSocket message. But for a PoC, sending fixed-size chunks works — the browser just needs to scan for `00 00 00 01` itself.

---

## Worker Changes

**None.** The proxy already forwards container → browser:

```typescript
containerWs.addEventListener("message", (event) => {
  if (server.readyState === WebSocket.OPEN) {
    server.send(event.data);  // already does this!
  }
});
```

---

## Browser Changes

Add to `src/pages/index.astro` (inside the existing `<script>`):

```javascript
// --- Preview decoder setup ---
let previewDecoder = null;
let previewCanvas = null;
let previewCtx = null;

function initPreview() {
  previewCanvas = document.createElement('canvas');
  previewCanvas.width = 640;
  previewCanvas.height = 360;
  previewCanvas.style.cssText = 'position:absolute;top:10px;right:10px;width:320px;height:180px;border:2px solid #0f0;z-index:1000;';
  document.body.appendChild(previewCanvas);
  previewCtx = previewCanvas.getContext('2d');

  previewDecoder = new VideoDecoder({
    output: (frame) => {
      previewCtx.drawImage(frame, 0, 0, previewCanvas.width, previewCanvas.height);
      frame.close();
    },
    error: (e) => console.error('[preview] decode error:', e)
  });
}

// --- Annex-B NAL parser ---
// Scans an ArrayBuffer for H.264 Annex-B start codes (00 00 00 01 or 00 00 01)
// and yields {data, isKeyFrame} for each NAL unit.
function* parseNALUnits(buffer) {
  const view = new DataView(buffer);
  let i = 0;
  while (i < buffer.byteLength - 4) {
    // Find start code
    let start = -1;
    while (i < buffer.byteLength - 4) {
      if (view.getUint32(i) === 0x00000001) {
        start = i + 4;
        i += 4;
        break;
      } else if (view.getUint16(i) === 0x0000 && view.getUint8(i + 2) === 0x01) {
        start = i + 3;
        i += 3;
        break;
      }
      i++;
    }
    if (start === -1) break;

    // Find next start code or end of buffer
    let end = buffer.byteLength;
    let j = start;
    while (j < buffer.byteLength - 4) {
      if (view.getUint32(j) === 0x00000001 ||
          (view.getUint16(j) === 0x0000 && view.getUint8(j + 2) === 0x01)) {
        end = j;
        break;
      }
      j++;
    }

    const nalType = view.getUint8(start) & 0x1f;
    const isKeyFrame = nalType === 5; // IDR slice

    yield {
      data: new Uint8Array(buffer, start, end - start),
      isKeyFrame,
      nalType
    };
    i = end;
  }
}

// --- Buffer incoming H.264 and decode ---
// ffmpeg outputs a continuous H.264 stream. We buffer until we have
// a complete frame (one or more NAL units), then decode.
let h264Buffer = new Uint8Array(0);

function appendH264(data) {
  const combined = new Uint8Array(h264Buffer.length + data.byteLength);
  combined.set(h264Buffer);
  combined.set(new Uint8Array(data), h264Buffer.length);
  h264Buffer = combined;

  // Try to extract complete frames
  const frames = [];
  // ... framing logic: group NAL units into Access Units ...
  // For simplicity, just decode each NAL unit as it arrives
  for (const nal of parseNALUnits(h264Buffer.buffer)) {
    if (nal.nalType === 7 || nal.nalType === 8) {
      // SPS (7) or PPS (8) — save for decoder config
      // TODO: extract and call previewDecoder.configure() if needed
    }

    const chunk = new EncodedVideoChunk({
      type: nal.isKeyFrame ? 'key' : 'delta',
      timestamp: performance.now() * 1000,
      data: nal.data
    });

    if (previewDecoder.state === 'configured') {
      previewDecoder.decode(chunk);
    }
  }
}

// --- Wire into existing WebSocket ---
// In your existing ws.onmessage handler, add:
ws.addEventListener('message', (event) => {
  if (event.data instanceof ArrayBuffer) {
    // Distinguish upstream vs downstream by size/content heuristic,
    // or better: prepend a 1-byte header on the container side.
    // For now: if it's from container, treat as H.264.
    // The browser only SENDS WebM, so any binary it RECEIVES must be H.264.
    appendH264(event.data);
  }
});
```

**Wait — how does the browser know a received binary message is H.264 vs WebM?** Simple: the browser only *sends* WebM chunks. Any binary message it *receives* on that WebSocket must be from the container, i.e., H.264.

---

## The Hard Parts (Why This Is "Sketch" Level)

### 1. Codec configuration
WebCodecs needs the SPS/PPS NAL units to configure the decoder:

```javascript
// Extract SPS (NAL type 7) and PPS (NAL type 8) from the first keyframe
// Build an avcC box:
const sps = ...; // Uint8Array
const pps = ...;
const avcc = new Uint8Array([1, sps[1], sps[2], sps[3], 0xff, 0xe1, ...]);

previewDecoder.configure({
  codec: 'avc1.42E01E', // from SPS
  description: avcc,
  optimizeForLatency: true
});
```

You need to buffer NAL units until you see SPS + PPS + IDR, then configure and start decoding. The container could also send a small JSON text message first with the codec string.

### 2. Frame vs NAL unit
A single video frame may be split across multiple NAL units (especially for large keyframes). You need to group NAL units into **Access Units** (frames) before calling `decode()`. The Annex-B stream uses `00 00 00 01` before every NAL unit. You group consecutive NAL units until the next `00 00 00 01` that starts a new non-VCL NAL type.

### 3. Backpressure
If the browser decode queue backs up, WebCodecs will throw `QuotaExceededError`. You need to check `decoder.decodeQueueSize` and pause reading from the WebSocket if it's too high.

### 4. Timing
The `timestamp` in `EncodedVideoChunk` is in microseconds. Since we're just showing a preview, `performance.now() * 1000` works, but for A/V sync you'd need proper PTS from ffmpeg.

---

## Even Simpler Alternative: Skip WebCodecs, Use a Second `<video>` with MSE

If WebCodecs feels too low-level, you can do the same pipeline but output **fMP4** from ffmpeg instead of raw H.264:

```bash
ffmpeg -i pipe:0 \
  -c:v libx264 -preset veryfast -tune zerolatency -g 30 -f flv rtmps://... \
  -c:v copy -f mp4 -movflags frag_keyframe+empty_moov pipe:3
```

Then in the browser, use **Media Source Extensions** (MSE):

```javascript
const mediaSource = new MediaSource();
const video = document.createElement('video');
video.src = URL.createObjectURL(mediaSource);

mediaSource.addEventListener('sourceopen', () => {
  const sourceBuffer = mediaSource.addSourceBuffer('video/mp4; codecs="avc1.42E01E"');
  
  ws.addEventListener('message', (e) => {
    if (e.data instanceof ArrayBuffer) {
      sourceBuffer.appendBuffer(e.data);
    }
  });
});
```

**Pros:** All modern browsers, no NAL parsing, handles framing automatically
**Cons:** 2-4s latency (MSE buffers), more complex state management

---

## My Recommendation

For this PoC: **don't build this yet.** Keep the Stream embed for validation. When you get to Stage 2 (Compositor) and need to see your mixed output in real-time, *then* add the raw H.264 preview. At that point, the value is much higher — you'll actually need sub-second feedback to position overlays.

If you do want to build it now, start with the **MSE/fMP4 approach** — it's 10x easier than WebCodecs and good enough (~2s latency vs 10-15s).
