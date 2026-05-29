#!/bin/bash
set -e

# Build and run the container locally (alternative to docker-compose)

echo "Building container..."
# Build from repo root so Dockerfile can access cf-logo.png
docker build -t media-workers-poc -f container/Dockerfile .

echo ""
echo "Starting container on port 8788..."
echo "Make sure STREAM_URL is set in your .env file"
echo ""

# Load STREAM_URL from .env if it exists
if [ -f .env ]; then
  set -a
  source .env
  set +a
fi

docker run --rm \
  -p 8788:8080 \
  -e STREAM_URL="${STREAM_URL}" \
  -e PORT=8080 \
  --name media-workers-container \
  media-workers-poc
