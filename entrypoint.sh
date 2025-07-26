#!/bin/sh

set -e

ARGS="-config-dir $CONFIG_DIR"
ARGS="$ARGS -port $PORT"
if [ ! -z "$LOG_LEVEL" ]; then
  ARGS="$ARGS -log-level $LOG_LEVEL"
fi
if [ ! -z "$DUPLICATE_CACHE_SIZE" ]; then
  ARGS="$ARGS -duplicate-cache-size $DUPLICATE_CACHE_SIZE"
fi

exec /bin/remix $ARGS
