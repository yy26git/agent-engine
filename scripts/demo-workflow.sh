#!/bin/sh
# End-to-end demonstration of the v2 WORKFLOW path and stage selection.
#
# Builds the real binary, pins the contract fixture, defines one profile per
# role, admits a v2 workflow, and runs a supervision pass. Everything happens
# under a temporary directory; nothing outside it is touched.
#
#   ./scripts/demo-workflow.sh
#
# This demonstrates STAGE SELECTION and ACTIVATION: the four role profiles are
# resolved and frozen at admission, then `run` creates a separate session per
# role. It is not a live agent: the Agent Deck boundary is the contract
# fixture (scripts/agent-deck-fixture), so no model is called and no real
# session is created. The reviewer never answers, so the request correctly
# stays nonterminal — that refusal is the point being shown.

set -e

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/ec-wfdemo.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

EC_EVENTS_CURSOR=${EC_EVENTS_CURSOR:-1}
export EC_EVENTS_CURSOR
EC_LOG="$WORK/deliveries.log"
export EC_LOG

say() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

say "1. build the binary"
cd "$ROOT"
GOCACHE=${GOCACHE:-/private/tmp/agent-engine-go-cache} go build -o "$WORK/agent-engine" ./cmd/agent-engine
printf 'built %s\n' "$WORK/agent-engine"

mkdir -p "$WORK"/store "$WORK"/src/project "$WORK"/evidence "$WORK"/out
printf 'package main\n\nfunc main() {}\n' > "$WORK/src/project/main.go"
chmod +x "$ROOT/scripts/agent-deck-fixture"

# The fixture is subject to the same pin gate as stock Agent Deck; see demo.sh.
FIXTURE_SHA=$(shasum -a 256 "$ROOT/scripts/agent-deck-fixture" | cut -d' ' -f1)
cat > "$WORK/runtime-lock.json" <<EOF
{
  "schema": 1,
  "repository": "agent-engine/scripts/agent-deck-fixture",
  "source_commit": "demo-fixture",
  "artifacts": {
    "$(go env GOOS)/$(go env GOARCH)": {
      "path": "$ROOT/scripts/agent-deck-fixture",
      "sha256": "$FIXTURE_SHA"
    }
  }
}
EOF

EC="$WORK/agent-engine --store $WORK/store"

say "2. write CONTROL.toml with one profile per role"
cat > "$WORK/store/CONTROL.toml" <<EOF
agent_deck_binary = "$ROOT/scripts/agent-deck-fixture"
profile = "wfdemo"
tool = "mcode"
output_root = "$WORK/out"
source_roots = ["$WORK/src"]
evidence_roots = ["$WORK/evidence"]
decision_authority_role = "auditor"

[limits]
max_repair_cycles = 3
max_worker_generations = 3
max_audit_attempts = 3
max_guidance_rounds = 2

[checkpoints]
soft_active_budget_seconds = 900
grace_seconds = 300
max_wall_seconds = 3600
review_timeout_seconds = 600
min_wake_interval_seconds = 60

[checks.build]
argv = ["/bin/sh", "-c", "exit 0"]
timeout_seconds = 30

[profiles.worker]
role = "worker"
tool = "mcode"
model = "DEMO_MODEL_WORKER"

[profiles.checkpoint]
role = "checkpoint"
tool = "codex"
model = "DEMO_MODEL_REVIEW"
effort = "high"

[profiles.diagnostic]
role = "diagnostic"
tool = "codex"
model = "DEMO_MODEL_REVIEW"
effort = "high"

[profiles.completion]
role = "reviewer"
tool = "codex"
model = "DEMO_MODEL_REVIEW"
effort = "high"
read_only = "true"
confinement_proven = "false"
EOF

say "3. the approved selections (launches nothing)"
$EC profiles --json
echo
echo "the unproven read-only reviewer is expected to be refused:"
$EC profiles --role reviewer --json 2>&1 | head -3

say "4. a task naming all four roles"
cat > "$WORK/workflow.json" <<EOF
{
  "version": 2,
  "request_id": "wfdemo-001",
  "origin": {"kind": "human", "caller_id": "operator"},
  "workspace": {"path": "$WORK/src/project"},
  "constraints": {"allow_code_changes": true},
  "input": {},
  "workflow_definition": {
    "version": 1,
    "name": "stage-selection-demo",
    "tasks": [
      {
        "id": "repair",
        "operation": "REPAIR",
        "objective": "Repair the reported failure",
        "instruction": "Inspect the failure, repair it and provide evidence.",
        "obligations": [
          {"id": "O-1", "criterion": "the build passes", "required_checks": ["build"]}
        ],
        "executor_profile": "worker",
        "checkpoint_profile": "checkpoint",
        "diagnostic_profile": "diagnostic",
        "reviewer_profile": "completion",
        "check_profiles": ["build"]
      }
    ]
  }
}
EOF
cat "$WORK/workflow.json"

say "5. admit the workflow (records work, launches nothing)"

# The unproven read-only reviewer refuses ADMISSION, not just launch. Showing
# that refusal is the point of the gate, so it is demonstrated before being
# crossed.
echo "with confinement_proven = false, admission is refused:"
$EC workflow admit --request-file "$WORK/workflow.json" --json 2>&1 | head -4

# A real deployment does NOT do this. It tests forbidden writes through the
# real selected tool, retains the evidence, and only then sets the flag. The
# fixture is not a confinement boundary, so the demo asserts nothing and says
# so: this concession is what a pilot must earn, not what it is given.
# The comment sits on its own line because this TOML subset requires a quoted
# string value with no trailing comment.
sed -i.bak 's/confinement_proven = "false"/# DEMO CONCESSION, not evidence - a pilot must earn this by testing.\nconfinement_proven = "true"/' \
  "$WORK/store/CONTROL.toml"
echo
echo "with the flag set (a real deployment must earn this by testing):"
$EC workflow admit --request-file "$WORK/workflow.json" --json

CHILD=$($EC workflow admit --request-file "$WORK/workflow.json" --json \
  | sed -n 's/.*"repair": "\([^"]*\)".*/\1/p')
echo "child: $CHILD"

say "6. the four selections, frozen at admission"
$EC status --request-id "$CHILD" --json | sed -n '/execution_profiles/,/^  }/p'

say "7. one supervision pass (run creates a session per role)"
$EC run --request-id "$CHILD" --once --json 2>&1 | head -20

say "8. which sessions were actually created, and when"
echo "--- add calls (the profile flag precedes the subcommand on the wire) ---"
grep 'agent-deck call: .* add ' "$EC_LOG" 2>/dev/null | sed 's/^agent-deck call: /  /' || echo "  (none)"
echo
echo "--- session ids minted by the fixture ---"
cat "$EC_LOG.sessions" 2>/dev/null | sed 's/^/  /' || echo "  (none)"
echo
echo "All four roles are activated by this one pass, each under its own"
echo "deterministic label, so no review role can borrow the worker's session."
echo
echo "--- deliveries ---"
grep -c '^DELIVERED to' "$EC_LOG" 2>/dev/null | sed 's/^/  /' || echo "  0"

say "9. the request is still not terminal, and says why"
$EC result --request-id "$CHILD" --json
echo
echo "The worker session reported itself finished, but the required build check"
echo "has no receipt, so no completion review is owed yet and no result is"
echo "published. Activity is not progress."

say "10. what a real reviewer must return"
echo "  a bare PASS is refused. The answer must echo the correlation envelope"
echo "  Agent Engine sent: request_id, kind, review_id, task_id, generation,"
echo "  answer_nonce — plus outcome and report_body."
echo
echo "  and it is submitted through the control layer, never by editing state:"
echo "    review-result --request-id <CHILD> --review-id <id> --kind COMPLETION"
echo "      --generation 1 --outcome PASS --report-file <report.md> --json"

say "done"
echo "store: $WORK/store"
