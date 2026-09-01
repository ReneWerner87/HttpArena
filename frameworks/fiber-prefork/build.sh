#!/usr/bin/env bash
set -euo pipefail

# Same source as the fiber entry, built with prefork switched on. The entry has
# no code of its own on purpose: the whole point of the pair is that the only
# difference between the two rows on the board is the process model.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
CONTEXT_DIR="$(cd "$SCRIPT_DIR/../fiber" && pwd -P)"
docker build -t httparena-fiber-prefork \
    --build-arg FIBER_PREFORK=1 \
    "$CONTEXT_DIR"
