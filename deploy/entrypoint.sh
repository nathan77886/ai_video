#!/bin/sh
set -eu

/app/ai-video &
api_pid=$!
nginx -g 'daemon off;' &
nginx_pid=$!

shutdown() {
    kill -TERM "$api_pid" "$nginx_pid" 2>/dev/null || true
}

trap shutdown INT TERM

while kill -0 "$api_pid" 2>/dev/null && kill -0 "$nginx_pid" 2>/dev/null; do
    sleep 1
done

status=0
if ! kill -0 "$api_pid" 2>/dev/null; then
    wait "$api_pid" || status=$?
fi
if ! kill -0 "$nginx_pid" 2>/dev/null; then
    wait "$nginx_pid" || status=$?
fi

shutdown
wait "$api_pid" 2>/dev/null || true
wait "$nginx_pid" 2>/dev/null || true
exit "$status"
