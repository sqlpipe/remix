#!/bin/sh

set -e

ARGS="-systems-dir $SYSTEMS_DIR -models-dir $MODELS_DIR"

if [ "$DEBUG" = "true" ]; then
  ARGS="$ARGS -debug"
fi

ARGS="$ARGS -port $PORT"

exec /bin/remix $ARGS
