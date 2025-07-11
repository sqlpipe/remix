#!/bin/sh

exec /bin/remix \
  -port $PORT \
  -systems-dir "$SYSTEMS_DIR" \
  -models-dir "$MODELS_DIR"
