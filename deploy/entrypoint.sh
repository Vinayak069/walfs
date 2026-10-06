#!/bin/sh
# Create the filesystem on first start, then mount it in the foreground
# (exposing Prometheus metrics). SIGTERM triggers a clean unmount + checkpoint.
set -eu
mkdir -p "$(dirname "$WALFS_IMAGE")" "$WALFS_MOUNT"
if [ ! -s "$WALFS_IMAGE" ]; then
  walfs mkfs -size "$WALFS_SIZE" -root-uid 0 -root-gid 0 "$WALFS_IMAGE"
else
  walfs fsck -replay "$WALFS_IMAGE" || echo "fsck reported problems; mounting anyway so recovery can run" >&2
fi
exec walfs mount -journal "$WALFS_JOURNAL" -metrics "$WALFS_METRICS" -allow-other "$WALFS_MOUNT" "$WALFS_IMAGE"
