#!/bin/sh
# Builds procdeck from the source in this folder, then starts the menu. The
# build is cached, so an unchanged source starts in about a second.
#
# The new binary is renamed over the old one: a supervisor still running the
# old binary keeps it, where writing into it would fail with "text file busy".
# That supervisor runs the old code until it is shut down and started with [8].
dir=$(cd "$(dirname "$0")" && pwd)
if ! command -v go >/dev/null 2>&1; then
    echo "Go not found. Install Go 1.26 or later, or put it on the PATH."
    exit 1
fi
(cd "$dir" && go build -o .procdeck.new .) || exit 1
mv -f "$dir/.procdeck.new" "$dir/procdeck" || exit 1
exec "$dir/procdeck" "$@"
