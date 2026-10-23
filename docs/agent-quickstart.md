# Coding-agent quickstart

Project and repository name: Agent Engine (`agent-engine`). Use the `agent-engine` executable and `AGENT_ENGINE_BINARY` setting. The protocol product identifier is `agent-engine`.

Agent Engine owns the report → review → plan → local acceptance → implementation →
verification → completion-review loop. A caller supplies requirements. It does
not need to reconstruct the engine or directly message worker/reviewer sessions.

For logs plus expected behavior, use an operator-registered **report-only** flow.
It runs one reporter and returns structured data, with no reviewer, planner, plan
acceptance or repair worker. Reporting can stay enabled while Forge code repair
is disabled. See [report-only setup and routing](report-only.md).

## Once per operator deployment

The operator supplies the absolute Agent Engine executable and store path, registers
flow templates, profiles and checks, authenticates providers, pins upstream
releases and qualifies the read-only reviewer/investigator. For trusted-local
workers the operator also grants the exact episode, workspace, expiry and budgets.
See [operator setup](trusted-local-caller-guide.md#operator-setup).

This grant is required when a writable worker uses trusted-local execution. In
this mode the worker's shell runs with the host user's permissions; Agent Engine
supervises the workflow but does not OS-confine those commands. Before starting,
the operator must pin a matching `[local_pilots.<episode-id>]` entry in the
store's `CONTROL.toml`, including the canonical workspace, worker profile,
expiry, maximum wall time and repair-cycle limit. For example:

```toml
[local_pilots.codex-feature-001]
workspace = "/absolute/prepared-worktree"
worker_profile = "minimax-acp-worker"
expires_at = "REPLACE_WITH_FUTURE_RFC3339_UTC"
max_wall_seconds = 7200
max_repair_iterations = 2
```

Use an expiry in the future and limits within the deployment's configured pilot
ceilings. Admission freezes the grant; editing `CONTROL.toml` later does not
extend an admitted episode. The grant authorizes bounded trusted-local execution
for that episode only. It is separate from accepting the generated plan, and it
does not authorize merge, push or mission launch. Read-only or OS-confined flows
follow their registered policy and do not need this trusted-local write grant.

For protocol-1 progress subscriptions, explicitly initialize deployment identity
once with `agent-engine --store /absolute/store deployment-init --json`. Verify the
selected executable with `version --json` and `capabilities --json` before using
the new watch flags. These read probes start no agents.

The store, qualification evidence and credentials stay outside worker workspaces.
New repair work belongs on a dedicated branch/worktree in the target repository.
Use `prepare-repair` or `prepare-repair-bundle` when the chosen flow requires it.
Finish Codex project trust before qualification. Export the same qualified
CODEX_HOME in the supervisor environment. A request cannot grant these permissions.

## Each task

The following commands are current, supported entry points. Replace absolute
paths and identifiers with operator-supplied values. `feature` is illustrative;
select an actual registered name from discovery.

### If you already have an implementation plan

Add `--plan-file /absolute/caller/implementation-plan.md` to the `episode prepare`
command below to import an existing plan. UTF-8 Markdown, plain text and JSON
documents are accepted as source text, up to 1 MiB. The request embeds the exact
content and its SHA-256 digest, so changing or deleting the external file later
does not change the admitted plan. Do this before admission; changed plan bytes
under an already admitted episode ID are refused.

Select a registered flow that includes PLAN and PLAN_ACCEPT. With an imported
plan, REPORT investigates its assumptions against the workspace.
The existing reviewer handles PLAN as a compatibility review and mapping into
the Engine's structured steps, rather than drafting a replacement. It must
preserve the supplied plan's work and acceptance criteria, link its steps to
the reviewed findings and admitted obligations, and quote original text for each
step. An incompatible plan stops as BLOCKED with the reason for revision; the
Engine does not automatically replace it. The PLAN label remains visible during
this review/mapping work.

At PLAN_ACCEPT, inspect and accept the exact structured plan digest. That plan
also pins the imported source digest; the original document is supplied alongside
the mapping to the worker and reviewers. `episode status`, progress events and
handover expose `imported_plan_sha256`. The import does not create a trusted-local
grant or widen the request's allowed paths, budgets or registered checks. Supply
the task's objective, criteria and scope with the normal prepare flags. Without
`--plan-file`, the normal workflow drafts a new plan.

For CLI/API callers constructing request JSON directly, use
`"imported_plan": {"name": "implementation-plan.md", "content": "...exact file text...", "sha256": "...hash of the UTF-8 content bytes..."}`.
Normal API request-body limits still apply. Check `capabilities --json` for
`imported-implementation-plan` before using an existing installed executable.
State written by this build uses schema 7; older executables refuse it.

To continue with a plan already retained by Agent Engine, resume the same episode
ID and inspect its recorded `plan_digest`; do not prepare a new episode.
Inspect/export the exact imported source separately with
`episode artifact --episode-id ID --imported-plan`; add `--json` for its name,
content and source digest. This read does not accept the structured plan.

```sh
/absolute/agent-engine --store /absolute/store flows --json
/absolute/agent-engine --store /absolute/store flows --template feature --json
```

Discovery shows phases, selected CLI/models/effort, checks, repair branch policy
and local plan acceptance. It reports configuration, not provider readiness.
Model selections come from profiles. Choose a different locally approved template
to use another worker; do not silently fall back to another provider.

Prepare a request with explicit, falsifiable criteria and change boundaries:

```sh
/absolute/agent-engine --store /absolute/store episode prepare \
  --episode-id codex-feature-001 --template feature \
  --workspace /absolute/prepared-worktree --caller-id codex-local \
  --objective 'Fix the addition regression without changing its tests.' \
  --criterion 'The original addition regression passes.' \
  --criterion 'The implementation preserves the existing public interface.' \
  --allow-code-changes --allowed-path 'src/**' --protected-path 'tests/**' \
  --output /absolute/caller/episode.json

/absolute/agent-engine --store /absolute/store episode validate \
  --request-file /absolute/caller/episode.json --json
```

Preparation copies registered verification check IDs and generates obligation
IDs. It does not invent criteria, authorize writes, create grants or admit work.
It refuses to overwrite a request file. Omit `--output` to print the request JSON.
For a prepared multi-repository bundle, include `--repair-bundle-sha256 DIGEST`.
Writable flows require explicit write permission and allowed paths. Read-only
flows omit `--allow-code-changes`. Extra obligation/check associations or limits
can be supplied by editing the generated JSON before validation/admission.

Validation checks fresh admission policy, workspace-specific reviewer receipts,
Codex home consistency, template checks, the MiniMax release receipt when used,
and the pinned Agent Deck artifact. It creates no requests or agents. JSON
refusals contain `code`, `responsible_actor`, `error` and `next_action`.
A passing preview is not a provider login/network probe, workspace lease claim or
permission to bypass later checks. Admission and launch recheck their own gates.

For a locally pre-authorized **single root card**, the operator can provision an
immutable program definition digest in `CONTROL.toml` and admit it through the
CLI. First compute the digest, then pin that exact digest under
`[program_authorities.<authority-ref>]` with the intended `program_id` and an
expiry. The digest command validates and prints identity; it does not grant
authority. The local grant is the authority boundary.

```sh
/absolute/agent-engine --store /absolute/store program digest \
  --definition-file /absolute/operator/program.json --json

# After the operator pins the returned definition_sha256 in CONTROL.toml:
/absolute/agent-engine --store /absolute/store program admit-card \
  --definition-file /absolute/operator/program.json --card-id card-1 \
  --request-file /absolute/caller/episode.json --json

/absolute/agent-engine --store /absolute/store run \
  --request-id codex-feature-001
```

This path admits one dependency-free card with no coordinator-owned resource
claims. It checks the program, card, source baseline, local template/profile,
scope, obligations and limits against the retained request and current local
policy. Multi-card definitions, dependency scheduling and shared-resource
ownership require the durable program coordinator and are refused here. Plan
acceptance semantics remain unchanged.

Launch from one terminal; observe from another:

```sh
/absolute/agent-engine --store /absolute/store episode launch \
  --request-file /absolute/caller/episode.json --json

/absolute/agent-engine --store /absolute/store episode watch \
  --episode-id codex-feature-001 --timeout 10m --events --json
```

Launch delegates to existing admission and `run`; it stays in the foreground as
the supervisor. Exact retries reconcile the same episode; changed contents under
the same ID are refused. Preserve request bytes for retry. Standard supervisor
shutdown/cancellation behavior applies. Do not run a second writer on the workspace.

Watch emits changed snapshots as NDJSON and starts no agents. It exits when the
request is terminal or awaits local plan acceptance. Timeout/interruption exits
3 and does not cancel execution. A zero watch exit may mean waiting for acceptance,
not PASS. Inspect the snapshot and durable result.

Submit once and retain the episode ID. Use one persistent observer; do not
repeatedly call status or drive while the supervisor owns execution. Silence
means no meaningful observed change. At `PLAN_ACCEPT`, implementation is waiting
for a local operator decision: show the plan and its exact digest, ask the user
once, and wait quietly. Do not self-approve or resubmit the episode. Observer
timeout does not cancel execution. After genuine acceptance, reconnect observation
of the same episode. Automatic chat wake-up requires a supported host adapter;
an ordinary CLI process cannot promise to wake an arbitrary chat.

Check the selected binary with `agent-engine version --json` and
`agent-engine capabilities --json` before using new conveniences. These probes need
no store and launch no agents. Operator setup initializes stable identity with
`agent-engine --store /absolute/store deployment-init --json`. Read approved flow,
CLI/model/effort selections with `agent-engine --store /absolute/store discover --json`.
Discovery reports configuration, not provider login or live availability.

## Plan acceptance and results

The local operator inspects the retained plan and accepts its exact digest using
`episode artifact` and `episode accept-plan --authorization-file`; see the
[authorization contract](trusted-local-caller-guide.md#codex-as-a-cli-caller).
Workers and reviewers cannot grant themselves acceptance. Keep the supervisor
running while the operator accepts, or resume the admitted request with `run`.

```sh
/absolute/agent-engine --store /absolute/store result --request-id codex-feature-001 --json
/absolute/agent-engine --store /absolute/store chats --request codex-feature-001 --json
/absolute/agent-engine --store /absolute/store cancel --request-id codex-feature-001 --json
```

Cancellation records intent; the supervisor owns reconciliation and cleanup.
Retain MiniMax desktop history for inspection after its owned ACP process stops.
Cloud conversation IDs are not interchangeable with local ACP session identities.
No automatic merge, commit or push is performed by these conveniences.

## Episode continuity handover

Before handing an episode to another coding-agent conversation, create or export
its current bounded handover:

```sh
/absolute/agent-engine --store /absolute/store episode handover \
  --episode-id codex-feature-001 --output /absolute/private/codex-feature-001-handover.json
```

The artifact is retained by content digest and the episode state points to the
latest committed handover. Re-running the command without state changes reuses
that artifact. Use `--digest SHA256` to export an older retained handover. The
record includes the admitted objective and obligations, report/plan and source
identities, bounded step/review/check history, non-secret pinned profile and
session identities, root-budget spent/remaining, and a next action. New
handover artifacts link the prior digest; each artifact includes the full
history currently retained in episode state.
The handover payload is capped at 1 MiB; if it cannot preserve the required
facts within that bound, creation refuses rather than truncating them. Export
files are created privately and never overwrite an existing path.

Continue by resuming the same episode ID and its existing bindings. A handover
does not approve a plan, release a workspace, rebind a role, or authorize a
replacement worker. Any replacement needs its own explicit local authorization
and proof that the prior writer has released the workspace.

For an active IMPLEMENT phase, an operator may request release of the exact
currently bound worker while the existing `run --request-id` supervisor is
still active. Read the digest from `episode status`, then submit it unchanged:

```sh
/absolute/agent-engine --store /absolute/store episode status \
  --episode-id codex-feature-001 --json
/absolute/agent-engine --store /absolute/store episode request-worker-release \
  --episode-id codex-feature-001 --expected-binding-sha256 SHA256
```

Run this while `run --request-id codex-feature-001` owns the episode; the CLI
refuses to queue release when no supervisor holds the ownership lock. The
request is durable and idempotent. Only the active supervisor can act on the
transport; it verifies the exact binding and process identity, refuses a
transport shared with another role, and records whether the worker was proven
released. A release request pauses implementation dispatch. It does not change
the pinned profile or binding, create a candidate, authorize a replacement, or
resume IMPLEMENT. Check `episode status` for `worker_release`; a `blocked`
result requires operator recovery and is not evidence of quiescence. The digest
goes stale if the episode phase, generation, session, backend, pinned profile,
accepted plan, or recorded process changes.

Before a live episode resumes, `run --request-id` reconciles owned child
processes and takes two stable source measurements before recovering a completed
review or admitting any transport. If source drift is found before any
implementation step and prior read-only work has settled, Agent Engine preserves the
old report and plan artifacts by digest, marks them inactive, records the return
to REPORT and pins the freshly measured source. The flow then requires a new
source-bound report, its independent review, a new plan and local acceptance of
that exact plan digest before implementation. A later drift repeats this
bounded, report-budgeted re-investigation. If a read-only turn or review is
still unsettled, or the report budget is exhausted, the episode remains blocked
for operator inspection. Once implementation has started, drift is
unattributed and blocks resume while preserving the accepted plan and history;
the current flow cannot infer whether those bytes came from an expected worker
write or another writer. A handover cannot clear that block or authorize a
replacement. The run command currently prepares all admitted role sessions
eagerly, so a worker session may be opened before PLAN_ACCEPT; it does not send
an implementation instruction until the exact plan has local acceptance.

## Command selection

| Need | Command |
|---|---|
| Registered flows/models/checks | `flows`, `profiles` |
| New episode JSON | `episode prepare` |
| Prerequisite preview without agents | `episode validate` |
| Admit and supervise an episode | `episode launch` |
| Resume existing supervision | `run --request-id` |
| Create or export an episode handover | `episode handover` |
| Request exact current worker release | `episode request-worker-release` |
| Observe only | `episode watch`, `episode status`, `chats` |
| Manually progress stored phase state | `episode drive` (advanced operator use) |
| Older ordinary request or workflow | `submit` / `start` / `workflow` (separate contracts) |

Forge and other apps continue through the existing loopback API. The thin Python
client in `examples/local-caller/client.py` handles submit/status/result/cancel;
the service owns supervision. It also supports authenticated `flows` and `validate`
calls through `GET /flows` and `POST /episodes/validate`, reusing the CLI projection
and preflight. Discovery is restricted to the endpoint’s allowed templates;
preflight enforces its caller namespace and starts no supervisor. It is not
another policy engine. Use this same
quickstart as the agent integration recipe; no new scheduler or upstream fork is
needed. Fully separate phase models and live Forge/Android acceptance remain the
limitations recorded in the remaining plan.

## Short invocation for coding agents

The thin adapter avoids repeating paths in every call. The operator supplies
these two locations explicitly; there is no implicit store lookup:

```sh
export AGENT_ENGINE_BINARY=/absolute/approved/agent-engine
export AGENT_ENGINE_STORE=/absolute/operator/store
sh /absolute/agent-engine/examples/local-caller/agent.sh flows --json
sh /absolute/agent-engine/examples/local-caller/agent.sh episode validate --request-file /absolute/caller/episode.json --json
```

The adapter forwards every argument to the same binary. It can invoke prepare,
launch, watch, result, chats and cancel without duplicating engine decisions.

## Progress subscriptions (protocol 1)

With the updated binary, the normal coding-caller recipe is one watch, not a
status loop. Keep the supervisor running in its own process:

```sh
/absolute/agent-engine --store /absolute/store episode watch --episode-id codex-feature-001 --events --json
```

This emits an initial snapshot and meaningful changes, then exits successfully
at a human decision or terminal state. A `waiting_for_operator: true` frame is
an action request, not completion: inspect `plan_digest` and `artifact_reference`,
ask the user once, and wait. Accept only through the existing local operator
mechanism. Do not turn the notification into an authorization file yourself.

Use `--follow` to keep that observer attached across approval. Use `--cursor`
with the last `event_id` to replay retained updates. An observation deadline exits
with code 3 without cancelling the episode. A reconnect does not launch anything.
A `resync-required` frame explicitly says older updates expired; its snapshot is
current state, not proof of complete historical delivery. Terminal notifications
still require reading `result`; they do not establish PASS themselves.

The HTTP client persists its cursor outside model context:

```sh
python3 /absolute/agent-engine/examples/local-caller/client.py \
  --url http://127.0.0.1:8787 --token-file /absolute/private/token \
  watch codex-feature-001 --cursor-file /absolute/private/observer.json --duration 3600
```

Its default also stops at a decision; add `--follow` to remain quietly connected.
The cursor parent must already exist and be operator-owned. Transport heartbeats
are comments and never model prompts. Keep one persistent tool wait in the
initiating agent's host. If that host closes or ends the chat turn, automatic
wake-up requires a separately supported host adapter; this version advertises
`automatic_chat_wakeup: false`. A persistent observer alone cannot reopen a chat.
If the Engine rejects a saved cursor after a deployment change, this client drops
only that cursor and reconnects once for a fresh snapshot; it never resubmits the
episode.

When using the local API, an observer credential can subscribe to
`GET /episodes/events` once and receive caller-scoped snapshots when any
workflow changes. The Engine owns the shared sampler and fans out updates; do not
poll `GET /episodes/ID` in a tight loop. Use `Last-Event-ID` when reconnecting,
and act on `requires_action`, `execution_health` and `next_action`. A stopped
supervisor or approval wait is not completion. The observer credential cannot
submit work, cancel it, or accept a plan.

On macOS, the optional floating HUD uses this same read-only observer stream. It
does not launch workers or services, approve plans, or control an episode. Build
it separately from the Go CLI with
`/absolute/agent-engine/scripts/build-agent-engine-hud.sh`, then open
`/absolute/agent-engine/dist/AgentEngineHUD.app`. Start the Engine service
explicitly with a separate `--observer-token-file`; enter the loopback endpoint
and token-file path in the HUD. The token value stays in the private file. If
the HUD closes or disconnects, the supervisor continues independently. Use
`agent-engine hud start`, `agent-engine hud status` and `agent-engine hud stop`
to manage only the companion app; those commands do not start or stop the Engine
service or any workflow.

Forge now negotiates `/capabilities`, subscribes to `/episodes/ID/events`, and
shows the exact-digest operator decision in its progress banner. Settings → AI →
Agent Engine → **Check Agent Engine** detects the executable and fetches caller-scoped
approved flow/CLI/model/effort selections. Save endpoint changes first. Detection
never starts a service, worker, login, or model turn. Provider availability stays
unknown until independently qualified. A detection failure does not silently pick
another model or revert to status polling.

For a local build, point `AGENT_ENGINE_BINARY` at the built executable and run
`version --json` and `capabilities --json` to verify it. Build instructions are in
[the README](../README.md#get-started).
