#!/bin/sh
set -eu

target=${1:?target copy is required}
baseline=${2:?baseline copy is required}
[ -f "$baseline" ]
cp "$baseline" "$target"
cmp -s "$target" "$baseline"
