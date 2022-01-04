#!/usr/bin/env bash
#
set -ex

rm -f context.tgz
tar czf context.tgz Dockerfile
python3 -m http.server
