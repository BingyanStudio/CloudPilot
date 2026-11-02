#!/bin/sh
set -eu

if [ "${1:-}" = --help ]; then
    exec node "$(dirname "$0")/build.mjs" "$@"
fi

builder_directory="$(mktemp -d)"
trap 'rm -rf "$builder_directory"' EXIT
npm install --prefix "$builder_directory" --cache "$builder_directory/npm-cache" --no-audit --no-fund ppio-sandbox@2.1.2 >&2
NODE_PATH="$builder_directory/node_modules${NODE_PATH:+:$NODE_PATH}" NODE_USE_ENV_PROXY=1 node "$(dirname "$0")/build.mjs" "$@"
