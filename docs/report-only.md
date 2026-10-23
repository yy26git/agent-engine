# Report-only workflows

Use this flow when Forge needs a semantic evaluation or report analysis of logs
and expected behavior, regardless of whether automatic code repair is enabled.
A report is data for Forge's existing semantic evaluator and report finalizer.
It is not an independently audited engineering verdict.

The implemented provider is Cursor CLI with an explicit model such as
`composer-2.5`. Existing repair flows and their provider choices are unchanged.
Agent Deck and OMA remain pinned, unmodified upstream dependencies.

## Operator setup

1. Install and authenticate Cursor CLI using `cursor-agent login`. Confirm
   `cursor-agent --list-models` includes the exact selected model.
2. Copy [the configuration example](../examples/cursor-report/CONTROL.toml.example)
   into a dedicated store's `CONTROL.toml`, or register its profile and template
   in an existing store. Supply canonical absolute paths and real digests.
   Use a versioned launcher path rather than the moving convenience symlink;
   calculate its SHA-256 with `shasum -a 256 /absolute/versioned/cursor-agent`.
   The launcher hash detects changes to that file; it is **not** a receipt
   proving every bundled dependency is immutable. Preserve the installed release.
3. Initialize deployment identity with
   `agent-engine --store /absolute/store deployment-init --json`.
4. Generate a private bearer token file outside all source roots (0600), then
   start the existing local service:

   ```sh
   agent-engine --store /absolute/store serve \
     --listen 127.0.0.1:8787 \
     --token-file /absolute/private/operator-token \
     --caller-prefix forge- --templates forge-semantic-report
   ```

Native Cursor `ask` mode is the vendor's read-only mode, **not OS confinement**.
The engine runs the reporter in a fresh empty temporary directory, supplies task
bytes over stdin, and passes no caller workspace to Cursor. The request cannot
supply executables, command arguments, environment overrides or a model. The
provider still runs as the local user and can access that user's credentials;
these measures do not establish a filesystem sandbox. Do not mark confinement
proven or use this report transport for repair, planning or independent approval.
The local operator explicitly enables this native report mode in policy.

## Forge routing

In Forge AI Workflows settings, connect the local endpoint, refresh discovery,
and select `forge-semantic-report` separately for **semantic-evaluation** and/or
**report-analysis**. Leave **code-repair** disabled if repairs are unwanted.
The registered investigation profile owns the CLI and model selection; Forge
cannot override them. This does not enable a disabled semantic-evaluation policy
or replace Forge's own semantic thresholds, citation validation or finalization.

The production `AiWorkflowService` / `AgentEngineWorkflowClient` sends one episode
containing `structured_task`: `function`, `schema_name`, `schema_json`, `system`
and `user`. The two supported function ids are `semantic-evaluation` and
`report-analysis`. Input is at most 512 KiB; output is one JSON object up to
2 MiB. JSON Schema validation runs locally, with external schema references
refused. Supply logs and expected semantic behavior in the task input; no tools
or repository investigation are needed. An episode still carries the caller's
approved workspace identity, but Cursor receives only the temporary directory.

A template must declare `report_only = "true"`, `read_only = "true"`, only
`REPORT`, and exactly one `investigation` role. It cannot declare checks, repair
routing or other phases. Admission freezes the task bytes and execution profile.
Writes, imported plans, verification commands and repair bundles are refused.
The positive legacy repair/audit limits in the example satisfy shared config
validation; they do not enable those phases. One report episode makes one model
call and does not retry malformed output automatically.

## Progress, results and failures

Forge subscribes to `/episodes/{id}/events`, reconnecting with the last cursor.
It does not repeatedly fetch episode status. Observation has the request's
existing timeout; cancelling or timing out asks the engine to cancel the provider.
The engine joins the process and checks its process group before publication.
A running provider has a durable pre-spawn intent and measured process record.
An interrupted report without retained output becomes BLOCKED rather than
silently starting another paid call. Unresolved process recovery also blocks.

Success is stage `REPORT_COMPLETE` with result `COMPLETE`, never `PASS`.
`GET /episodes/{id}/output` returns the input digest, exact JSON output and its
digest, admitted profile/model, provider session id and optional reported model.
A missing provider-reported model is recorded as absent, never copied from the
selection. Forge checks task identity and output digests before consuming data.
A negative semantic finding can be a successfully completed report.

Completed output is reused on a repeat launch; malformed output, process failure,
cancellation and deadline expiry never become report completion. A new report
attempt requires a new episode identity, making further cost explicit.

## Validation record — 2026-10-18

The live synthetic test used the production Forge client, built Engine loopback
service and authenticated Cursor CLI `2026.09.28-64d2043`, selecting Composer 2.5.
It reported target loss at log:3 and hold at log:4 instead of continued following,
with automatic repair false. There was one event subscription and no status
polling, reviewer, planner or repair launch. Evidence is retained locally at
`agent-engine-live-tests/report-only-20261018-01/acceptance.json` beside the
submitted request and returned report. This sample does not establish Android,
simulator or real hardware acceptance, or OS confinement.

Fixture validation: Go build/vet and the full Go suite passed; changed-package
race checks passed after an initial expired-context lifecycle test failure and
its successful focused rerun. The final report-specific race checks also pass.
Forge contracts (128), orchestrator (257), workflow-service (5) and settings UI
(3) tests passed, with desktop typecheck clean. No live provider is invoked by
those suites. Changes are on `codex/report-only-cursor` in both repositories.
