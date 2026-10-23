#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
package="$repo_root/desktop/macos/AgentEngineHUD"
output="$repo_root/dist"
if [ "$#" -ge 1 ]; then output=$1; fi
app="$output/AgentEngineHUD.app"
temporary_build=$(mktemp -d "${TMPDIR:-/tmp}/agent-engine-hud.XXXXXX")
trap 'rm -rf "$temporary_build"' EXIT HUP INT TERM

if [ "$(uname -s)" != "Darwin" ]; then
  printf '%s\n' 'AgentEngineHUD can only be built on macOS.' >&2
  exit 2
fi
mkdir -p "$output"
mkdir -p "$temporary_build/module-cache"
swiftc -parse-as-library \
  -module-cache-path "$temporary_build/module-cache" \
  -framework AppKit -framework SwiftUI -framework Security \
  -o "$temporary_build/AgentEngineHUD" \
  "$package/Sources/AgentEngineHUDCore/ObserverStreamPolicy.swift" \
  "$package/Sources/AgentEngineHUD/AgentEngineHUDApp.swift" \
  "$package/Sources/AgentEngineHUD/EpisodeDiscovery.swift" \
  "$package/Sources/AgentEngineHUD/HUDRootView.swift"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
install -m 0755 "$temporary_build/AgentEngineHUD" "$app/Contents/MacOS/AgentEngineHUD"
install -m 0644 "$package/Info.plist" "$app/Contents/Info.plist"
codesign --force --sign - --timestamp=none "$app"
printf 'Built %s\n' "$app"
