# Operations

How to run `agent-engine` against a real project, and what each failure
means.

For workflow tasks, configure [stage execution profiles](profile-selection.md)
before admission. They select each task's CLI/model and the checkpoint,
diagnostic and completion reviewers. Their activation is automatic; the manual
`bind` examples below remain the legacy v1 path. Runtime upgrades follow the
[upstream maintenance gate](upstream-maintenance.md).

## Setup

### 1. Create a store

The store is the operator-approved writable artifact root. It must be **outside**
any read-only source snapshot, or evidence and subject become the same bytes.

```sh
mkdir -p ~/.local/share/agent-engine
```

### 2. Write `CONTROL.toml`

This file is operator-owned and is never supplied by a request. A request may
*name* a check; only this file decides what that name means.

```toml
agent_deck_binary = "agent-deck"
profile = "default"
tool = "mcode"

output_root = "/Users/you/src/myproject/.ec-out"
source_roots = ["/Users/you/src/myproject"]
evidence_roots = ["/Users/you/src/myproject/.ec-evidence"]

# Only this role may ACCEPT a decision. Empty means nobody may, so every
# decision stays a proposal — the safe direction.
decision_authority_role = "auditor"

[limits]
max_repair_cycles = 3
max_worker_generations = 3
max_audit_attempts = 3
max_guidance_rounds = 2

[checkpoints]
# Deployment policy, not contract constants. There is no universal
# "every fifteen minutes" that is right for every project.
soft_active_budget_seconds = 900
grace_seconds = 300
max_wall_seconds = 14400
review_timeout_seconds = 1800
min_wake_interval_seconds = 60

[checks.build]
argv = ["go", "build", "./..."]
timeout_seconds = 600

[checks.regression]
argv = ["go", "test", "-json", "-count=1", "./..."]
timeout_seconds = 1800
# exit 0 having discovered no tests has verified nothing
discovery_required = "true"
```

A missing or invalid file refuses every command. That is deliberate: a control
layer with no policy is a control layer that would take its rules from whoever
called it.

### 3. Authenticate MiniMax (you do this, not `agent-engine`)

A `minimax-acp` profile launches `mcode acp` and reuses **the login you already
have**. Agent Engine never runs a login, never reads an auth-state file, never copies
one, and never reads, stores, logs or hashes your token or API key. Your
credential stays with your own `mcode` installation.

Pick the account you actually have — these are two separate deployments with
separate logins:

```sh
# China account
mcode login

# Global account
mcode login --region global
```

Then choose the credential source `mcode` should read, and confirm the result:

```sh
mcode provider use token-plan      # or: mcode provider use api-key
mcode provider list                # provider is active, enabled, hasApiKey: true
```

Non-interactive hosts add `--no-browser` to the login step.

`mcode provider list` reports **whether** a credential exists (`hasApiKey:
true|false`), never the credential itself. That boolean is the only
authentication signal Agent Engine reads.

Tell `CONTROL.toml` which region your login belongs to, under the ACP profile:

```toml
[profiles.minimax-worker]
role = "worker"
tool = "mcode"
transport = "minimax-acp"
model = "MINIMAX_PROVIDER/MINIMAX_MODEL"
region = "cn"          # or "global" — required, never defaulted
```

`region` is required for `minimax-acp` profiles and may only be `cn` or
`global`. An unknown value is **refused at load**, not corrected: the two
deployments authenticate separately, so silently assuming one turns into an
authentication error at the first turn that looks like a transport fault and
sends you debugging the wrong thing. It is pinned into the request at
admission, so editing it later cannot re-aim an in-flight request.

Agent Deck profiles do not need `region` — they reach their own
already-authenticated CLI (`codex login`).

## Everyday commands

```sh
EC="agent-engine --store /abs/store"

# Admit a request. Idempotent for identical bytes; a conflict for anything else.
$EC submit --request-file request.json --json

# Say WHO is working. The two roles must be different sessions, or the
# completion audit would be the implementer reviewing itself.
$EC bind --request-id my-request \
    --worker  sess-abc123 \
    --auditor sess-def456

# Supervise it. --once processes the current backlog and exits; without it the
# run continues until Ctrl-C, and stopping is safe at any point.
$EC run --request-id my-request --once --json

# Or admit and run in one step.
$EC start --request-file request.json --once

# Where is it?
$EC status --request-id my-request --json

# The durable result, once there is one.
$EC result --request-id my-request --json
```

From the worker and reviewer side:

```sh
# Evidence must be digest-pinned.
$EC evidence --request-id R --id E-1 --path diff.patch --sha256 <64 hex>

# A typed checkpoint. Advances nothing on its own.
$EC checkpoint --request-id R --payload-file cp.json --json

# A failed attempt, counted against the objective cycle budget.
$EC attempt --request-id R --cycle-id c-1 --check regression \
    --output "FAIL: TestFoo ..." --hypothesis "the scheduler" --json

# A reviewer's correlated answer. --report-file is required for an approving one.
$EC review-result --request-id R --review-id A-1 --kind COMPLETION \
    --generation 1 --outcome PASS --report-file report.md --json

$EC question --request-id R --title "which approach?" --json
$EC decision --request-id R --title "use the queue" --accept-as auditor --json

$EC cancel --request-id R --reason "operator changed their mind" --json
```

## Reading the output

`status` reports the stage, the open obligations and their dispositions, the
repair/audit/guidance counters and the effective limits. A cycle counter at its
ceiling with the stage still `WORKING` means the request is waiting on a
diagnosis, not that it is progressing.

`result` is the only thing that reports an outcome. Before the request is
terminal it returns `{"ready": false, "stage": "..."}` — that is not an error, and
it is never replaced by a fabricated result.

## What each refusal means

| Refusal | Meaning and next step |
|---|---|
| `--store is required` | There is no implicit lookup by design. Pass the store. |
| `request_id already admitted with different content` | Two incompatible things share an ID. Nothing was started. Decide which one you meant. |
| `this role may not accept decisions` | `decision_authority_role` is unset, or names someone else. A proposal was recorded instead. |
| `a completion PASS must cite a committed report` | Supply `--report-file`. The reasoning is part of the approval. |
| `the event bus is disabled` | The supervisor would see nothing while appearing healthy. Fix `AGENTDECK_EVENTS_BUS`. |
| `the bus no longer retains frames back to the recorded cursor` | Evidence was lost. Progress is refused rather than resumed over a gap. |
| `an action was already in flight and its outcome is unknown` | A previous process may or may not have delivered it. Confirm manually; do not resend. |
| `workspace is not inside an approved source root` | Add the root to `source_roots` deliberately, or fix the request path. |
| `check "x" is not in the operator registry` | A required check that does not exist never runs. Define it or drop it. |
| `profiles.X.region is required for a minimax-acp profile` | Your ACP profile does not say which account it targets. Add `region = "cn"` or `"global"`, then run `mcode login` or `mcode login --region global` yourself. |
| `must be one of cn, global` | A region was misspelled. Fix the spelling; it is refused rather than guessed, because a wrong region fails later as an auth error that reads like a transport fault. |
| `credential source is not usable as observed` | `mcode provider list` reports the provider inactive, disabled, without a credential, or on an unsupported source. Run `mcode provider use token-plan\|api-key`. Agent Engine reuses your login and never reads the credential. |
| `a VERIFYING receipt was taken against different content` | The source moved. Re-run the check; do not approve on the old receipt. |
| `another engine already owns this request` | One active engine per request. The previous one has exited. |
| `no worker is bound` | Run `bind --worker <id> --auditor <id>` first. |
| `no auditor is bound` | A worker with no independent reviewer accumulates audit debt nothing can discharge. |
| `the implementer and the reviewer must be different sessions` | One session cannot review its own work. |
| `the event bus is disabled` | Supervision would see nothing while appearing healthy. Fix `AGENTDECK_EVENTS_BUS`. |

## What `run` does, in order

Per observed frame, and this ordering is the design:

1. **Decide** from the frame, using the session binding as it is *now* — not as
   it was when the run started, so a replacement worker is visible immediately.
2. **Commit** the decision, the effect it owes and the bus cursor in **one**
   state transaction. A crash between them would either record an action nobody
   took, or skip a frame that was never acted on.
3. **Dispatch** the effect immediately, while the follow is live. Draining only
   on shutdown would leave a long-running supervisor recording decisions and
   telling nobody.

`--once` captures the current bus head as a boundary and stops after it. A run
that is stopped with Ctrl-C is safe: state is durable and the request is simply
not finished, which `status` will say.

## Diagnosing a stuck request

Work top-down. Each step answers a different question.

1. **Is anything running?** `status`. A stage that is not advancing is the
   symptom; the counters say whether budget is being consumed.
2. **Is the reviewer reachable?** An open review with no receipt means either the
   instruction was not delivered or the reviewer has not answered. Check the
   effect ledger in `STATE.json` for the effect's status and its `send_id`.
3. **Is the failure objective or a repeat?** `attempt` reports
   `repeated_signature` and `guidance_exhausted`. A repeated signature means the
   same check failed the same way — renaming the hypothesis does not buy a new
   attempt.
4. **Is verification the blocker?** Missing, failed and stale are three different
   problems: run it, repair it, re-run it.
5. **Did the content move under a receipt?** Compare `content_digest` on the
   receipt against the current snapshot.

## Recovery and rollback

**Restarting the supervisor is normal.** It reconciles in-flight effects through
Agent Deck's own send-status interface before doing anything new: landed becomes
delivered, a known failure becomes retryable, still-in-flight is left with the
queue, and an unprovable outcome is **blocked** rather than resent.

**A blocked effect needs a human.** The state carries the effect ID, the target
and the reconciliation note. Confirm whether the message arrived, then either
`workflow`-equivalent reconcile by editing nothing and re-running after removing
the ambiguity, or submit a new request. There is deliberately no flag that
force-replays an ambiguous delivery.

**Rolling back a store.** Stop the engine first. A version-2 state file is not
assumed readable by an older build, so restore a pre-change snapshot under the
workspace lease rather than letting a downgrade attempt to interpret it. Never
delete `REQUESTS/<id>/` to "reset" a request: its identity is what stops a retry
from relaunching work that already happened.

**Retention.** Request directories and their results are not deleted
automatically. A retry whose result was reclaimed would relaunch work.

## Known limitations

- **No sandbox is provided.** See `contracts.md`. Read-only execution is refused
  unless confinement is demonstrated by the environment.
- **The event stream is not guaranteed complete.** Queue drops, opt-in event
  sources and daemon restarts can all lose frames. Absence of a frame is UNKNOWN.
- **The external verifier seam is a contract, not an implementation.** No real
  harness is wired. A harness that both repairs and verifies is refused, because
  the thing that changed the content cannot vouch for the change.
- **Independence is a separate session plus evidence reinspection**, not
  cryptographic proof that a local caller is honest.
