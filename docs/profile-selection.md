# CLI, provider and model selection

Agent Deck and OMA remain pinned upstream dependencies. These selections live in
Agent Engine's operator configuration and workflow contract; no upstream source edit
or fork is required.

## Select every task and review stage

The operator defines named profiles in `CONTROL.toml`. Each profile chooses a
CLI/tool, provider configuration, model, reasoning setting and launch scope.
Requests reference approved names rather than supplying executable commands.

| Stage | Workflow field | Profile role | Execution |
|---|---|---|---|
| Investigation, implementation, repair, report task | `executor_profile` | `worker` | Selected CLI and model in the task's worker session |
| Checkpoint review | `checkpoint_profile` | `checkpoint` | Separate review session |
| Stuck-work diagnosis | `diagnostic_profile` | `diagnostic` | Separate diagnostic session |
| Completion approval | `reviewer_profile` | `reviewer` | Separate completion-review session |
| Verification | `check_profiles` | Operator check registry | Deterministic commands; no model |
| Outer orchestration | Pinned OMA process backend | Scheduler | Executes Agent Engine; no model call |

Every task, including a report task, can name different profiles. For example:

```json
{
  "id": "repair",
  "operation": "REPAIR",
  "objective": "Repair the reported failure",
  "instruction": "Inspect the failure, repair it and provide evidence.",
  "executor_profile": "minimax-worker",
  "checkpoint_profile": "checkpoint-review",
  "diagnostic_profile": "diagnostic-review",
  "reviewer_profile": "completion-review",
  "check_profiles": ["build", "regression"]
}
```

This is a task fragment inside the existing v2 workflow request. Workflow
validation still requires its objective, instruction source and other contract
rules. A name must exist and be approved for the corresponding role.

For compatibility, omitted checkpoint and diagnostic profile names select the
completion review profile. Automatic activation still creates separate sessions
for the three review roles. New workflows should name all four selections.
Legacy v1 requests retain manual worker/auditor binding. Old state without these
selections continues to load; it does not acquire invented model provenance.

## Current agent entry point

Use [the agent quickstart](agent-quickstart.md) and `flows --json` for episode
selection. MiniMax engineering workers use stock `mcode acp`. Interactive custom
`mcode` launch presets below are historical configuration examples and remain
blocked for supervised MiniMax work. See the caller guide for ACP/pilot setup.

## Configure a profile

For a tool whose model/reasoning options stock Agent Deck supports, use `tool`,
`model`, and `effort`. Agent Engine passes them through the public `add --model` and
`--effort` options; unsupported selections fail rather than falling back.
`sandbox` also reaches the real launch path; it does not by itself prove read-only
confinement. Worktree profiles are refused: stock Agent Deck's worktree option
requires a branch, and Agent Engine also needs to bind verification to that worktree
instead of the original workspace. The previous boolean setting did neither.
Do not use a worktree as a substitute for enforced read-only scope.

For a custom CLI or another provider behind an existing CLI, use an approved
launch preset:

```toml
[profiles.minimax-worker]
role = "worker"
tool = "mcode"
provider = "minimax"
model = "OPERATOR_APPROVED_MODEL_SELECTOR"
launch_command = ["/absolute/path/to/mcode", "--model", "{model}"]
launch_env = ["HOME=/absolute/isolated/minimax-worker-home"]
```

Replace the path and model selector with values verified for the installed CLI
and account. The `mcode` tool must be registered through stock Agent Deck's
operator custom-tool configuration, including its status/prompt detection. The
installed MiniMax Code 0.6.2 interactive parser supports `--model`; this example
does not claim live authentication, status events or task completion have been
proven. `mmx` is a provider configurator; `mcode` is the coding agent.

The isolated HOME must already contain the operator-approved configuration and
authentication for that CLI. Agent Engine does not install credentials, copy a user's
home, install hooks, or reconfigure an existing global CLI. Environment overrides
are session-local and persist in Agent Deck's wrapper through start/restart.
Put configuration locations in `launch_env`; keep credentials out of it because
profile values are recorded in state and may appear in diagnostic arguments.

`launch_command` is an array of literal arguments. Its executable must be an
absolute path. `{model}`, `{effort}`, and `{provider}` expand as single quoted
shell arguments; they cannot introduce shell syntax. Custom launch presets must
consume `{model}`. If `effort` is selected, they must also consume `{effort}`.
The preset replaces the generated tool command using stock `add --wrapper`;
native model/effort flags are then omitted to avoid sending them to an
unsupported custom-tool path.

Provider identifies the approved backend, while the command, model selector and
isolated CLI configuration actually select it. A provider label alone cannot
switch backends. A non-native provider without an explicit launch preset is
refused. CLI binaries and referenced configuration files remain operator-owned;
the admitted profile freezes argument/environment values, not those files'
contents. Verify and retain their versions/configuration for live acceptance.

MiniMax Code 0.6.2's interactive parser does not expose `--effort`; its `exec`
mode does. Do not attach the headless effort flag to an interactive session or
substitute `mcode exec` without a tested messaging/status adapter. To require a
reasoning level there, supply an approved adapter that enforces it and pass the
effort through that adapter. An omitted effort is reported as a runtime default,
not evidence of a selected reasoning level.

## Inspect and execute selections

```sh
agent-engine --store /absolute/store profiles --json
agent-engine --store /absolute/store profiles --role checkpoint --json
agent-engine --store /absolute/store workflow admit --request-file workflow.json --json
agent-engine --store /absolute/store status --request-id CHILD_REQUEST_ID --json
agent-engine --store /absolute/store run --request-id CHILD_REQUEST_ID
```

`profiles` lists the approved choices. Its `launchable` field means local profile
validation passed, not that a live provider or confinement test passed. `status`
shows admission-pinned `execution_profiles`, model/effort and each role's bound
session. `run` and OMA's `task-exec` activate those selections before supervision.
An existing manually bound session cannot stand in as proof it used the profile.

Admission freezes all four role selections. Changing a named profile affects
new admissions, not an admitted task's activation or replacement worker. Repeat
activation reconciles its owned sessions and does not create duplicates. Review
routing uses the session assigned to that review kind; a missing explicit role
cannot silently borrow the completion reviewer. Cancellation includes all roles.

Reviewer output must return the JSON correlation supplied in its instruction:
`request_id`, `kind`, `review_id`, `task_id`, `generation`, `answer_nonce`, plus
`outcome` and `report_body`. The broker measures the answering session and checks
the correlation before accepting it. Bare PASS/FAIL output and old review output
are refused. Existing pending reviews need answers in this format as well.

## Acceptance still required

The source and process-boundary tests verify configuration, pinning, dispatch,
role routing, recovery and cancellation. They are not live-provider acceptance.
Before a live pilot, inspect the pinned CLI/model/reasoning selection and requested model echo,
credentials, event producer and status gates, messaging, forbidden-action
confinement, current verification receipts and clean shutdown. The existing
G1/G2/G3/G9/G13 gates are not closed by this change.

See [upstream maintenance](upstream-maintenance.md) for dependency upgrades and
upstream contributions.

## OpenAI reasoning effort (2026-10-13)

Model and effort are separate profile fields. For a Codex profile, choose
`model = "gpt-6-luna"` and `effort = "xhigh"` for Extra High, or
`effort = "max"` for Max. These are different values; Agent Engine preserves the
selected value through admission and launch. Omitting effort preserves the CLI
default. Editing a profile affects new admissions, not already pinned tasks.

The installed Codex catalogue advertises Low, Medium, High and Extra High for
gpt-6-astra, gpt-6.1-sol, gpt-6-sol, gpt-6-luna, gpt-5.6-sol,
gpt-5.6-terra, gpt-5.6-luna and gpt-5.5. All except gpt-5.5 also advertise Max.
Ultra is an additional Codex delegation setting for Astra, Sol and Terra;
it is unavailable for Luna and gpt-5.5 and is never an OpenAI API effort.
Known unsupported combinations are refused without substituting Medium.
Unknown model ids remain selectable with a safe protocol effort value; the
installed CLI determines their support. MiniMax and other providers keep their
own effort semantics.

Forge exposes the same selection separately under AI Backends → Function
routing → Reasoning effort. Its Codex choices and default label come from the
installed catalogue; API choices follow model-specific official documentation.
Saved route effort reaches prompt proposal, repair and structured API requests.
An omitted setting means provider default, which may vary by model and client.
Selected effort is configuration provenance, not measured answer-turn identity.

Sources: [OpenAI reasoning guide](https://developers.openai.com/api/docs/guides/reasoning),
[GPT-6 Luna model](https://developers.openai.com/api/docs/models/gpt-6-luna),
[GPT-5.5 model](https://developers.openai.com/api/docs/models/gpt-5.5), and
the installed `codex debug models --bundled` catalogue inspected on 2026-10-13.

## Operator model-inspection policy (2026-10-11)

Missing answer-turn model metadata is nonblocking. Admission still freezes the
selected model, and a measured conflicting answer model is refused. Model-authored
`model_echo` is a display field, never substituted into observed attribution.
Existing role instructions request the selected label (for example `M2.7`).

`agent-engine --store /absolute/store chats --request REQUEST_ID` lists retained role
sessions and ACP receipt paths. Native MiniMax history remains available after
attachments and processes shut down. Open the printed `mcode --session` command
after supervision stops; reading receipts does not create a process.
