#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
package="$repo_root/desktop/macos/AgentEngineHUD"
temporary_build=$(mktemp -d "${TMPDIR:-/tmp}/agent-engine-hud-tests.XXXXXX")
trap 'rm -rf "$temporary_build"' EXIT HUP INT TERM

swiftc -parse-as-library \
  -module-cache-path "$temporary_build/module-cache" \
  -o "$temporary_build/observer-fixtures" \
  "$package/Sources/AgentEngineHUDCore/ObserverStreamPolicy.swift" \
  "$package/Tests/ObserverPolicyFixtureRunner.swift"
"$temporary_build/observer-fixtures"
