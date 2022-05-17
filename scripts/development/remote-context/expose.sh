#!/usr/bin/env bash
#
set -e

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null && pwd )"
cd "$SCRIPT_DIR" || exit 1

tmpdir=$(mktemp -d)
tar -czf "$tmpdir/context.tgz" Dockerfile
python3 -m http.server --directory "$tmpdir"

trap "{ rm -rf $tmpdir; }" SIGINT SIGTERM ERR EXIT
