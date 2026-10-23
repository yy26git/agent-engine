# Contracts

`agent-engine` accepts engineering requests and publishes their durable
results. It is a separate Go module and reaches Agent Deck only through its
public CLI and NDJSON event stream.

Two rules govern everything here.

**Every command requires `--store <absolute path>`.** A request is identified by
its store *and* its request ID, never by the current working directory. There is
no hidden index and no implicit lookup, so two stores holding the same request ID
can never be confused for one another, and a caller cannot accidentally act on
whichever store happens to be nearby.

**Machine JSON goes to stdout; diagnostics go to stderr.** A caller can pipe any
command's stdout into a JSON parser without stripping a preamble. With `--json`,
even a refusal is machine-readable on stdout (`{"ok": false, "error": "..."}`)
while the human-readable form still goes to stderr.

## Request

```json
{
  "version": 1,
  "request_id": "forge-run-1842-repair",
  "origin": {"kind": "forge", "caller_id": "local-forge"},
  "operation": "REPAIR",
  "workspace": {"path": "/absolute/approved-worktree", "expected_head": "abc123"},
  "objective": "Fix the attributable regression without changing protected evaluation code",
  "obligations": [
    {"id": "O-1", "criterion": "Regression reproduced, fixed and verified",
     "required_checks": ["build", "regression"]}
  ],
  "evidence": [
    {"path": "/absolute/approved/run/result.json", "sha256": "<64 hex>", "kind": "external-run"}
  ],
  "constraints": {
    "allow_code_changes": true,
    "allowed_paths": ["src/**"],
    "protected_paths": ["evaluation/**"]
  },
  "verification": [{"check_id": "build"}, {"check_id": "regression"}],
  "limits": {"repair_cycles": 3, "worker_generations": 3, "audit_attempts": 3}
}
```

### Admission rules

These are refusals, not repairs. A rejected request leaves no trace on disk.

| Rule | Why |
|---|---|
| `version` must be 1 | A newer schema may carry a safety field this build would ignore. |
| Unknown fields are refused | A misspelled `allowed_paths` would otherwise be accepted as unenforced scope. |
| `request_id` is a single path segment, `[A-Za-z0-9._-]{1,128}` | It becomes a directory name. Two requests must not be able to collide. |
| `operation` ∈ {INVESTIGATE, REPAIR, VERIFY, GENERATE_REPORT} | Defaulting would guess at whether code may change. |
| `workspace.path` must be absolute and `..`-free, and lie under an approved source root | Two spellings of one directory must not take two workspace leases. |
| At least one obligation, with a non-empty ID and criterion | A request with nothing to be complete against cannot be completed or failed. |
| Evidence paths absolute, under an approved evidence root, with a 64-hex digest | A path alone points at bytes that may since have changed. |
| `REPAIR` requires `allow_code_changes: true`; every other operation requires false | Read-only is a capability, not a prompt. A repair with no write permission cannot do anything. |
| No path may be both allowed and protected | Otherwise the grant is resolved by whichever check ran last. |
| Every named check must exist in the operator registry | A check that does not exist never runs, and a required check that never runs is not satisfied. |
| Requested limits may not exceed the operator ceiling | A caller may ask for less work, never more. |

### Identity

`request_sha256` is a canonical digest of the whole request: object keys sorted,
array order preserved, no safety field omitted. Object keys are sorted so
re-serialisation cannot change identity; array order is preserved because
obligations, evidence, allowed and protected paths are semantically ordered.

The digest is what makes admission idempotent:

- **Same `request_id`, same digest** → the existing request is returned. Nothing
  new is started, and the state revision does not move.
- **Same `request_id`, different digest** → conflict. Nothing is started, and
  nothing on disk is modified.

## Result

```json
{
  "version": 1,
  "request_id": "forge-run-1842-repair",
  "request_sha256": "<digest>",
  "task_id": "T-<digest prefix>",
  "status": "PASS",
  "ready": true,
  "summary": "obligations met and an independent completion audit passed",
  "findings": [],
  "code_changed": false,
  "verification": [{"check_id": "build", "receipt_id": "V-job-1", "outcome": "PASS", "snapshot_id": "S-..."}],
  "completion_audit": {"review_id": "A-1", "outcome": "PASS", "report_id": "A-1-answer"},
  "artifacts": [{"path": "REPORT.md", "sha256": "<digest>"}],
  "finished_at": "2026-10-05T09:00:00Z"
}
```

`status` is one of `PASS`, `FAIL`, `BLOCKED`, `CANCELLED`. These are not
interchangeable:

- **PASS** — obligations met, required checks satisfied against a current
  snapshot, and an independent completion audit passed.
- **FAIL** — evidence disproved an obligation after permitted recovery.
- **BLOCKED** — required evidence, access, identity or safe recovery could not be
  established. A missing build tool is BLOCKED, not FAIL: the evidence could not
  be produced, and calling that a failure would blame the work for a harness
  problem and consume a repair cycle that belonged to the actual defect.
- **CANCELLED** — cancellation was requested and its effects reconciled.

Before a request is terminal, `result` returns `{"ready": false, "stage": ...}`
with a reason. It never returns a partial or empty result object, because a
caller mistaking a nonterminal stage for a finished request is the most damaging
way this contract could lie.

Once published, a result's identity and outcome never change. A later correction
is a new request.

## Stages

```
READY → WORKING → CHECKPOINT_REVIEW → WORKING
       → VERIFYING → COMPLETION_AUDIT → VERIFIED
       → DIAGNOSTIC (guidance) → WORKING
       → REFRESHING → WORKING
       → BLOCKED / CANCELLED / FAILED
```

Only a **COMPLETION** review can approve. A CHECKPOINT review answers "should we
keep going?"; a DIAGNOSTIC answers "why is this stuck?". Neither can verify
anything, and that is enforced by the outcome allowlist per review mode rather
than by a rule someone has to remember at each call site.

## Store layout

```
<store-root>/
  CONTROL.toml                       operator policy: tools, roots, checks, limits, authority
  STORE.lock                         admission lock
  WORKSPACE-LEASES/<digest>.lock     one lease per canonical workspace
  REQUESTS/<request-id>/
    REQUEST.json                     immutable admitted request and its digest
    STATE.json                       the sole mutable authority
    RESULT.json                      regenerable projection of the committed result
    REPORTS/<report-id>.md           immutable reasoning bodies
```

`STATE.json` is the only mutable authority. `RESULT.json` is a projection and may
be regenerated after a crash; the committed body under `REPORTS/` is what a reader
trusts, and only through a digest state recorded itself.

## What the control layer does not claim

- **It does not implement a sandbox.** Read-only execution requires confinement
  that can be *demonstred*. A source hash detects a write to a read-only tree; it
  does not prevent one. A same-user `chmod` is not a boundary from the same user.
  A workspace lease serialises cooperating callers and does nothing about a
  process that ignores it.
- **It does not infer progress from activity.** Commits, changed files, green
  commands and status flips are activity. Progress is a reviewer's evidenced
  judgement about a named criterion.
- **It does not promise exactly-once external delivery.** An effect is committed
  before it is attempted, marked delivered only on a transport receipt, and an
  outcome that cannot be proven is **blocked**, never resent.
- **It does not treat absence of events as absence of activity.** The event stream
  is not guaranteed complete. Missing frames are UNKNOWN and require reconciliation.
