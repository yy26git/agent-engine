#!/bin/sh
# Thin agent adapter. The operator supplies identity/location; Agent Engine owns policy.
set -eu
: "${AGENT_ENGINE_BINARY:?Set AGENT_ENGINE_BINARY to the approved absolute Agent Engine executable}"
: "${AGENT_ENGINE_STORE:?Set AGENT_ENGINE_STORE to the approved absolute operator store}"
case "$AGENT_ENGINE_BINARY" in /*) ;; *) echo 'AGENT_ENGINE_BINARY must be absolute' >&2; exit 2 ;; esac
case "$AGENT_ENGINE_STORE" in /*) ;; *) echo 'AGENT_ENGINE_STORE must be absolute' >&2; exit 2 ;; esac
exec "$AGENT_ENGINE_BINARY" --store "$AGENT_ENGINE_STORE" "$@"
