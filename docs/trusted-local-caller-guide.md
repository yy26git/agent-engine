# Local callers and shell-capable pilot episodes

For everyday tasks, start with [the agent quickstart](agent-quickstart.md). This
guide contains operator setup and the lower-level integration contracts.

Codex, Forge and another local application can submit to the same Agent Engine
CLI/API. They select a locally registered workflow template and supply an
objective, falsifiable obligations, constraints and verification requirements.
The existing Engine launches the template's pinned worker/reviewer profiles.
Neither caller JSON nor a model's plan supplies executable commands or grants.

## Where repair changes belong

Agent Engine contains the controller. A Forge repair patch belongs in the Forge
repository; an Android repair patch belongs in the Android repository. Each
target gets a new repair branch and a separate Git worktree. The original
checkout, branch and uncommitted files stay untouched.

Add `repair_branch_prefix = "agent-engine/repair/"` to the locally approved writable
Forge template `forge-repair`. Its endpoint must allow that template. The default
Forge adapter uses this template; it never supplies a worker CLI or model.
In Forge Settings → AI → Agent Engine, set **Repair worktree parent** to an existing
dedicated directory. Configure Agent Engine `source_roots` to include that directory
and both original repositories. Agent Engine enforces those local roots; the Forge
setting only selects where to request preparation. The destination's parent
must already exist. Then prepare both repositories together:

```sh
cat > /tmp/forge-repair-bundle.json <<'JSON'
{
  "episode_id": "forge-repair-case-123",
  "template": "forge-repair",
  "destination": "/absolute/approved-worktree-parent/forge-repair-case-123",
  "targets": [
    { "id": "forge", "repository": "/absolute/Forge", "base": "HEAD" },
    { "id": "android", "repository": "/absolute/Android Operating System", "base": "HEAD" }
  ]
}
JSON

agent-engine --store /absolute/store prepare-repair-bundle \
  --request-file /tmp/forge-repair-bundle.json --json
```

The response freezes each symbolic base to a commit and returns a durable
`receipt_sha256`. An episode names the bundle destination as `workspace.path` and
that digest as `repair_bundle_sha256`. Admission checks the locally retained
receipt and pins each target's branch, base and Git common directory. Exact
retries recover the same preparation; unrelated existing paths are refused.
Dirty original files are not included: choose the intended committed base
explicitly. The worker edits only the `forge/` and `android/` worktrees. Branch
drift or namespace/symlink replacement refuses execution and approval; no
automatic merge, commit or push occurs.

Forge's Agent Engine adapter requests the bundle through its authenticated local API,
verifies the returned Git identities, submits the receipt-bound episode,
and runs Forge/Android validation in the prepared worktrees. It records before
and after observations for the original checkouts and refuses if they change.
The selected failed scenario still needs a fresh live run against the prepared
revisions before the Forge pilot is accepted.

The trusted-local mode uses normal worker shell tools with host-user permissions.
It is functional supervision, not filesystem containment or unattended security
qualification. Status and final results report `execution_assurance: trusted-local`
separately from the engineering verdict. Qualified read-only reviewers and the
existing check/audit/correlation gates still apply.

## Operator setup

Use the existing [profile setup](profile-selection.md),
pin Agent Deck and the actual worker release, and qualify Codex read-only profiles
for the exact workspace. Keep the store, evidence, receipts and bearer token
outside the worker workspace. For MiniMax, use the stock ACP transport and the
desired advertised selector. `effort` selects `thinkingEffort` when advertised;
`permission_mode`, if explicitly set, must also be advertised and read back.
There is no automatic provider/model fallback or no-fork policy.

For Codex native delivery, export the qualified CODEX_HOME before launching
Agent Engine, not only inside the profile wrapper. Enable Agent Deck's native recall
for delivery/answer evidence through its public configuration CLI. The qualified
home and selected upstream binary must match the roles' launch_env:

```sh
export CODEX_HOME=/absolute/qualified-codex-home
/absolute/pinned/agent-deck -p YOUR_PROFILE config set recall.enabled true --json
```

`run` checks the frozen Codex profiles against the supervisor's home, and `serve`
checks its allowed templates before opening the endpoint. A mismatch refuses
with the required `CODEX_HOME`; it cannot start an ACP worker while Agent Deck
is looking for the Codex transcript in another home. Cancellation remains
available for an already admitted request.

For a newly prepared worktree, finish Codex's local project-trust setup for that
exact path **before** running qualification and admission. Its first interactive
launch can otherwise add a `projects.<path>.trust_level` entry to the qualified
home's `config.toml`, changing the configuration digest and correctly refusing
the next role's launch. Keep the profile's explicit read-only sandbox flags.
Retain the configuration digest before qualification and compare it after
interactive startup. If it changes, cancel the admitted episode and qualify the
final configuration for a fresh admission; do not replace its pinned receipt.

If direct provider networking fails but an existing operator-controlled local
HTTP proxy works, pin HTTP_PROXY and HTTPS_PROXY in each affected Codex profile's
launch_env. Only http://literal-loopback:port URLs without credentials or routing
components are accepted. Changing that environment invalidates its receipt:
re-run qualify through the exact selected tool before admitting another episode.
This configures network routing; it does not create filesystem authority.
Production ACP startup has a 90-second handshake allowance within the episode's
remaining wall-clock deadline. Cancellation skips new transport initialization.

Register caller-specific templates in `CONTROL.toml`. For example, a repair
template using existing role mappings is:

```toml
[episode_templates.codex-feature]
phases = ["REPORT", "REPORT_REVIEW", "PLAN", "PLAN_ACCEPT", "IMPLEMENT", "VERIFY", "COMPLETION_REVIEW"]
roles = ["investigation=codex-investigator", "worker=minimax-acp-worker", "reviewer=codex-completion"]
required_checks = ["build", "regression"]
return_edges = ["REPORT_REVIEW=REPORT", "IMPLEMENT=IMPLEMENT", "VERIFY=IMPLEMENT", "COMPLETION_REVIEW=IMPLEMENT"]
max_repair_iterations = 1
read_only = "false"
repair_enabled = "true"
```

`IMPLEMENT=IMPLEMENT` lets an independent implementation assessment return work
for a bounded repair. `repair_enabled = "true"` requires explicit bounded routes
for each rejection source present in the template: report review to report,
implementation assessment to implementation, verification to implementation,
and completion review to implementation. The registry refuses a partial route
set; it never fills in missing edges. Discovery reports `repair_enabled` and the
effective `rejection_disposition` (`bounded_return_edges` or `stop_blocked`).

For compatibility, a template with no `return_edges` and no
`repair_enabled = "true"` remains usable as a fail-stop workflow. If a report,
implementation, verification or completion review rejects work, Agent Engine
records a durable stop marker and publishes a terminal BLOCKED result with the
reason and next action for the local operator. The next run can finish that
publication after a crash without dispatching another phase. A legacy template
with any `return_edges` opts into repair routing and must now declare the
complete route set relevant to its declared phases. To retain stop-on-rejection
behavior for such a template, remove its return edges; no route is inferred.

An optional template `max_wall_seconds` sets the locally approved episode allowance;
omitting it (or zero) retains the one-hour default. Admission caps the allowance
by the pilot ceiling and freezes it; a larger pilot grant alone does not extend
the default. Negative values are refused. Configuration changes never extend an
already admitted episode.

Every referenced profile and check must already be locally approved. Another
template may choose a Codex worker and different checks/requirements. PLAN,
report review and completion currently share that template's reviewer profile;
fully separate per-phase model overrides are not implemented.

Authorize one pilot request locally, replacing paths and expiry before use:

```toml
[local_pilots.codex-feature-001]
workspace = "/absolute/disposable/project"
worker_profile = "minimax-acp-worker"
expires_at = "REPLACE_WITH_FUTURE_RFC3339_UTC"
max_wall_seconds = 900
max_repair_iterations = 1
```

The workspace must exist and fit `source_roots`. Ceilings must fit the local
`checkpoints.max_wall_seconds` and `limits.max_repair_cycles`. The grant is keyed
to the exact episode ID and template worker. Admission freezes its canonical
workspace, profile identity, expiry and caps with the episode root. Editing
configuration cannot change an admitted pilot. Expiry blocks starts/resume and
bounds the CLI run deadline; cleanup remains available. Old admissions do not
gain permission. Do not set a confinement proof flag to enable the pilot.

## Codex as a CLI caller

Codex can save an episode request and run:

```sh
/absolute/path/agent-engine --store /absolute/operator/store episode admit \
  --request-file /absolute/caller/episode.json --json
/absolute/path/agent-engine --store /absolute/operator/store run \
  --request-id codex-feature-001
```

An example request shape is:

```json
{
  "version": 1,
  "episode_id": "codex-feature-001",
  "origin": { "kind": "agent", "caller_id": "codex-local" },
  "workspace": { "path": "/absolute/disposable/project" },
  "objective": "Implement the specified behavior without changing its evaluation criteria.",
  "template": "codex-feature",
  "obligations": [
    { "id": "behavior-correct", "criterion": "The original regression test passes.", "required_checks": ["regression"] }
  ],
  "constraints": { "allow_code_changes": true, "allowed_paths": ["src/**"], "protected_paths": ["tests/**"] },
  "verification": [ { "check_id": "build" }, { "check_id": "regression" } ]
}
```

The supervisor runs report -> review -> plan and waits for local plan acceptance.
Use `episode artifact --episode-id ID --digest DIGEST` to inspect the retained
plan. An authorized local operator/runner uses `episode accept-plan` with its
digest-bound authorization file. Worker/reviewer profiles cannot self-accept.
Run `episode status --episode-id ID --json`, `chats --request ID --json` and
`result --request-id ID --json` for state, session identities and final evidence.

For MiniMax inspection, request the selected model's literal label in the same
worker chat; the engine supplies this instruction. After owned ACP cleanup,
manually open the retained conversation in MiniMax Code desktop history. Keep
one execution owner and retain history; automatic exact-chat opening is unproven.

## Separate local API callers

Run loopback endpoints with distinct private token files and caller namespaces,
sharing the same operator store and engine implementation:

```sh
env CODEX_HOME=/absolute/qualified-codex-home /absolute/path/agent-engine --store /absolute/operator/store serve \
  --listen 127.0.0.1:8787 --token-file /absolute/operator/forge.token \
  --caller-prefix forge- --templates forge-repair

env CODEX_HOME=/absolute/qualified-codex-home /absolute/path/agent-engine --store /absolute/operator/store serve \
  --listen 127.0.0.1:8788 --token-file /absolute/operator/codex.token \
  --caller-prefix codex- --templates codex-feature,codex-refactor \
  --observer-token-file /absolute/operator/codex-observer.token
```

These commands are separate long-lived terminals/services, not one shell script.
Tokens must be private regular files with at least 32 bytes. Template names must
be registered locally. Omitting `--templates` preserves the existing behavior:
all locally registered templates are permitted for that endpoint.

An observer token is optional and separate from the operator token. It can read
the caller-scoped `GET /episodes` snapshot, the shared `GET /episodes/events`
discovery stream, per-episode progress events and durable results. It cannot
submit, resume, cancel or accept plans. Keep its file outside approved source
roots with mode 0600. The service samples authoritative episode state on one
shared two-second cadence and fans out changed snapshots; clients hold one
stream and reconnect with `Last-Event-ID`. Reconnects receive a current
snapshot, with `resync_required` when the event identity changed.

```sh
umask 077
openssl rand -hex 32 > /absolute/operator/codex-observer.token
chmod 600 /absolute/operator/codex-observer.token
```

The [thin example client](../examples/local-caller/client.py) works from Codex,
another local application, or a terminal. It only sends HTTP data to the existing
engine; it has no execution or approval authority. For example:

```sh
python3 /absolute/agent-engine/examples/local-caller/client.py \
  --url http://127.0.0.1:8788 --token-file /absolute/operator/codex.token \
  submit /absolute/caller/episode.json

python3 /absolute/agent-engine/examples/local-caller/client.py \
  --url http://127.0.0.1:8788 --token-file /absolute/operator/codex.token \
  status codex-feature-001
```

Use `flows` to discover endpoint-approved templates and `validate FILE` for a
read-only prerequisite preview. The authenticated routes are `GET /flows` and
`POST /episodes/validate`; neither starts agents or admits work.

Use `result ID` or `cancel ID` through the same client. The client refuses remote
URLs and redirects, and does not automatically retry submission. A replay uses
the same episode ID and exact request bytes; changing the contents under that
ID is refused by admission.

Send bearer-authenticated `POST /episodes` requests, then use `GET /episodes/ID`,
`GET /episodes/ID/result` and `POST /episodes/ID/cancel`. Submission, status,
supervisor resume, result access and cancellation enforce the caller namespace.
For each request, the frozen local pilot grant is still required for unqualified
MiniMax ACP execution. A caller cannot send its own pilot grant or arbitrary flow
program. The service does not turn a terminal stage into a result without the
durable result receipt. Workspace/request ownership prevents duplicate writers.

CLI access is trusted operator access, not isolation between operating-system
users. Ordinary cleanup does not guarantee recovery from every detached job or
supervisor crash. Such qualification, optional OMA outer scheduling and remote
relay execution are separate later work.

ACP terminal receipts use their own send identity; notification cursors are
separate connection ordinals. Real notification loss blocks decisions. This ACP
adapter cannot replay notifications after a saved cursor, so it refuses that
resume instead of pretending to continue. Do not clear a gap or create a new
chat under the old request to bypass recovery.

A trusted-local cancellation can reconcile an already-retired ACP host without
relaunching it when durable process identities agree and the process group is
also absent. A surviving shell tool, pending spawn or uncertain identity keeps
cancellation unresolved. Already-stopped Agent Deck sessions are verified through
public inventory on retry. Retained MiniMax chat history is preserved by cleanup.


## Proven alternate caller/model run

R6's Codex-facing Python client completed a sample episode through this API with
MiniMax M3.1 Flash Preview at max effort, plus independent Codex report and
completion reviews. This was a synthetic sample, not acceptance of a production application. Model choice remains a local registered
profile/template decision; the caller submits requirements rather than a CLI.

A live supervisor holds the Store's canonical primary/bundle-target workspace
leases. A conflicting episode can be admitted, but cannot start roles while the
first holds its workspace; its caller can still cancel it. These cooperative
leases do not provide OS write confinement. Exact resubmission keeps the same
episode/task, and status can resume a disconnected supervisor. Plan acceptance
remains a separate local operator action.

## Resumable progress and discovery

The updated service exposes authenticated `GET /capabilities`, `GET /catalog`,
`GET /episodes`, `GET /episodes/events` and `GET /episodes/ID/events`. The
catalogue and episode snapshot are scoped to the endpoint's caller namespace and
allowed templates. Observer projections expose pinned profile/model/effort
labels without launch commands or environment values, and do not claim provider
authentication.

Initialize deployment identity once with `deployment-init`. The event endpoint
requires that identity and checks episode namespace/template access. Supply the
last SSE `id` as `Last-Event-ID` after reconnect. Cursors bind deployment, admitted
request digest and progress sequence; wrong identity, future or malformed cursors
are refused. A retention gap emits `resync-required` with a current snapshot.
The bundled Python watch client handles a stale deployment cursor by reconnecting
without that cursor for a fresh snapshot; this affects observation only and never
repeats episode submission.
The atomic STATE.json retains the latest 256 metadata updates, each bounded to
16 KiB; plans, evidence and results retain their existing authoritative storage.

Per-episode progress connections have a five-minute lifetime, 15-second comment
heartbeats, a ten-second write deadline and a 64-observer limit. The service-wide
discovery stream has one two-second state sampler and fans out changed snapshots
to connected callers. Clients reconnect with backoff and retained cursor. Disconnects
and observer timeouts never cancel work. Explicit cancellation and existing
execution deadlines remain separate. Delivery is at least once: an output followed
by a crash before its cursor receipt may be delivered again.

The Python `watch` client and CLI `episode watch --events` are persistent caller
adapters. Forge main consumes the SSE stream and forwards validated typed progress
to its renderer. Automatic arbitrary-chat wake-up is unsupported; keep a host tool
wait active or provide a separately qualified host adapter. Human plan acceptance
always uses the existing exact-digest local authorization contract. Full Forge
process restart is not an automatic repair-controller restart: `resumeObservation`
reattaches an existing adapter identity without admitting or starting execution.
