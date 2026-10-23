# Agent Engine floating HUD

This optional macOS companion is a standalone SwiftUI view hosted by an
AppKit floating panel. The Go CLI and local API do not depend on Swift or Xcode.

Build on macOS 13 or later with Xcode command-line tools:

```sh
./scripts/build-agent-engine-hud.sh
mkdir -p "$HOME/Applications"
cp -R dist/AgentEngineHUD.app "$HOME/Applications/"
agent-engine hud start
```

The CLI also discovers the fixed `dist/AgentEngineHUD.app` bundle when run
from this checkout. For an installed CLI, keep the companion at
`~/Applications/AgentEngineHUD.app` or `/Applications/AgentEngineHUD.app`.
Inspect or stop only the companion with `agent-engine hud status` and
`agent-engine hud stop`. These commands do not start the Engine service, a
workflow, a login item, or any provider process. They do not pass observer
token values or event-supplied executable paths to the app.

Before connecting, initialize the Engine deployment and start its loopback
service with an optional observer token:

```sh
agent-engine --store /absolute/operator/store deployment-init
agent-engine --store /absolute/operator/store serve \
  --listen 127.0.0.1:8787 \
  --token-file /absolute/private/operator.token \
  --observer-token-file /absolute/private/hud-observer.token \
  --caller-prefix local- --templates approved-flow
```

Open Details, enter the local endpoint and the observer-token file path, then
connect. The token itself stays in the private file. The HUD accepts loopback
HTTP only, subscribes to the shared discovery stream, and reconnects using its
saved event cursor. It reports unknown/unavailable usage rather than inferring
billable tokens from context occupancy or elapsed time.

The floating panel is read-only. It does not start or control workflows. Closing
it ends observation only; reopening reconnects to the service. Its expanded
state, selected workflow and screen position are saved locally. The service and
CLI continue to work without the HUD installed.

Run the observer-policy fixtures on macOS with
`sh scripts/test-agent-engine-hud-fixtures.sh`. They exercise the production
request, stream-size and cursor helpers without launching the HUD or connecting
to an Engine service. They do not measure native-window focus behavior, idle
CPU/RSS, monitor recovery or end-to-end independence; those still require a
named Mac acceptance run.

In Details, **Copy episode ID** copies the selected episode's identifier.
**Copy read-only status command** copies an `episode status` command for that
episode. Both actions are enabled only while the episode is present in the
authenticated local discovery snapshot. The command is copied as text and is
never run by the HUD; replace `/absolute/store` with the service's store path
before running it in a terminal. The HUD does not receive that store path, so
it cannot safely fill it in or open an authenticated browser inspection URL.
