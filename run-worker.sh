#!/bin/bash
set -e

echo "Cleaning stale Wrangler deploy config..."
rm -rf .wrangler/deploy

echo "Starting Wrangler dev server..."
npx wrangler dev dist/server/entry.mjs
