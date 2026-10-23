#!/bin/sh
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
output_dir="$project_root/bin"
if [ "$#" -gt 0 ]; then output_dir="$1"; fi
mkdir -p "$output_dir"
output_dir=$(CDPATH= cd -- "$output_dir" && pwd)
cd "$project_root"
go build -buildvcs=false -o "$output_dir/agent-engine" ./cmd/agent-engine
printf '%s\n' "$output_dir/agent-engine"
