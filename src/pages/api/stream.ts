import type { APIRoute } from "astro";

// This must be server-rendered to handle WebSocket upgrades
export const prerender = false;

export const GET: APIRoute = async ({ request }) => {
  const upgradeHeader = request.headers.get("Upgrade");
  if (!upgradeHeader || upgradeHeader.toLowerCase() !== "websocket") {
    return new Response("Expected Upgrade: websocket", { status: 400 });
  }

  const [client, server] = new WebSocketPair();
  server.accept();
  server.binaryType = "arraybuffer";

  const containerUrl = "ws://localhost:8788/ws";

  let containerWs: WebSocket;
  let messageQueue: Array<{ data: any; isBinary: boolean }> = [];
  let containerOpen = false;

  function flushQueue() {
    if (!containerOpen) return;
    while (messageQueue.length > 0) {
      const msg = messageQueue.shift()!;
      if (msg.isBinary) {
        containerWs.send(msg.data);
      } else {
        containerWs.send(msg.data);
      }
    }
  }

  try {
    containerWs = new WebSocket(containerUrl);
    containerWs.binaryType = "arraybuffer";

    containerWs.addEventListener("open", () => {
      containerOpen = true;
      flushQueue();
    });

    containerWs.addEventListener("message", (event) => {
      if (server.readyState === WebSocket.OPEN) {
        server.send(event.data);
      }
    });

    containerWs.addEventListener("close", (event) => {
      containerOpen = false;
      if (server.readyState === WebSocket.OPEN) {
        server.close(event.code, event.reason);
      }
    });

    containerWs.addEventListener("error", (error) => {
      console.error("[proxy] Container WebSocket error:", error);
      containerOpen = false;
      if (server.readyState === WebSocket.OPEN) {
        server.close(1011, "Container connection error");
      }
    });

    server.addEventListener("message", (event) => {
      const data = event.data;
      const isBinary = !(typeof data === "string");

      if (containerOpen) {
        containerWs.send(data);
      } else {
        // Queue messages until container is connected.
        messageQueue.push({ data, isBinary });
      }
    });

    server.addEventListener("close", (event) => {
      if (containerWs.readyState === WebSocket.OPEN) {
        containerWs.close(event.code, event.reason);
      }
    });

    server.addEventListener("error", (error) => {
      console.error("[proxy] Client WebSocket error:", error);
      if (containerWs.readyState === WebSocket.OPEN) {
        containerWs.close(1011, "Client connection error");
      }
    });
  } catch (error) {
    console.error("[proxy] Failed to connect to container:", error);
    server.close(1011, "Failed to connect to container");
  }

  return new Response(null, {
    status: 101,
    webSocket: client,
  });
};
