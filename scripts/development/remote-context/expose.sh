#!/usr/bin/env bash
#
set -e

tmpdir=$(mktemp -d -t hephaestus)
tar -czf "$tmpdir/context.tgz" remote-context/Dockerfile
python3 -m http.server --directory "$tmpdir"

trap "{ rm -rf $tmpdir; }" SIGINT SIGTERM ERR EXIT
