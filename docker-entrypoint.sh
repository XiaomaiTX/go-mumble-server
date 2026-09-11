#!/bin/sh
set -e

# Root start (default): fix /data ownership for bind mounts whose host dir is
# owned by an arbitrary UID, then drop privileges to the mumble user.
# Non-root start (docker run --user): exec directly.
if [ "$(id -u)" = "0" ]; then
    chown mumble:mumble /data 2>/dev/null || true
    find /data -maxdepth 1 -exec chown mumble:mumble {} + 2>/dev/null || true
    exec gosu mumble:mumble ./go-mumble-server "$@"
fi

exec ./go-mumble-server "$@"
