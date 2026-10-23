#!/bin/sh
# End-to-end demonstration of agent-engine.
#
# Builds the real binary, creates an isolated store, admits a request, binds the
# two roles, and runs one supervision pass against the Agent Deck contract
# fixture. Everything happens under a temporary directory; nothing outside it is
# touched.
#
#   ./scripts/demo.sh
#
# This demonstrates the SUPERVISION LOOP — observation, decision, a durable
# effect, a transport receipt and a correlated review. It is not a live agent:
# the Agent Deck boundary is a contract fixture (scripts/agent-deck-fixture), so
# nothing here proves a real session can be supervised end to end.

set -e

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/ec-demo.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

EC_EVENTS_CURSOR=${EC_EVENTS_CURSOR:-1}
export EC_EVENTS_CURSOR
EC_LOG="$WORK/deliveries.log"
export EC_LOG

say() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

say "1. build the binary"
cd "$ROOT"
go build -o "$WORK/agent-engine" ./cmd/agent-engine
printf 'built %s\n' "$WORK/agent-engine"

mkdir -p "$WORK"/store "$WORK"/src/project "$WORK"/evidence "$WORK"/out
printf 'package main\n\nfunc main() {}\n' > "$WORK/src/project/main.go"
chmod +x "$ROOT/scripts/agent-deck-fixture"

# The fixture must be PINNED, exactly like the production runtime.
#
# VerifyPin refuses any executable that is not the artifact an approved lock
# names, so the fixture is subject to the same gate as stock Agent Deck: a
# demo that could launch an unreviewed binary would prove nothing about the
# gate it is demonstrating. This lock is written next to the built binary,
# which locateRuntimeLock searches before the working directory — otherwise
# the real runtime-lock.json in the repo root would be found instead and
# would (correctly) refuse this fixture.
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

say "2. write CONTROL.toml (operator policy)"
cat > "$WORK/store/CONTROL.toml" <<EOF
agent_deck_binary = "$ROOT/scripts/agent-deck-fixture"
profile = "demo"
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
EOF
cat "$WORK/store/CONTROL.toml"

say "3. submit a request"
cat > "$WORK/request.json" <<EOF
{
  "version": 1,
  "request_id": "demo-001",
  "origin": {"kind": "human", "caller_id": "operator"},
  "operation": "REPAIR",
  "workspace": {"path": "$WORK/src/project"},
  "objective": "Make the build green and record the evidence",
  "obligations": [
    {"id": "O-1", "criterion": "the build passes", "required_checks": ["build"]}
  ],
  "constraints": {
    "allow_code_changes": true,
    "allowed_paths": ["**"],
    "protected_paths": ["evaluation/**"]
  },
  "verification": [{"check_id": "build"}],
  "limits": {"repair_cycles": 3}
}
EOF
$EC submit --request-file "$WORK/request.json"

say "4. submit it again (must be idempotent, not new work)"
$EC submit --request-file "$WORK/request.json"

say "5. run with nothing bound (must refuse, not idle silently)"
$EC run --request-id demo-001 --once || true

say "6. bind the two roles"
$EC bind --request-id demo-001 \
  --worker  sess-worker-1 \
  --auditor sess-auditor-1

say "7. run one supervision pass"
$EC run --request-id demo-001 --once

say "8. what the reviewer actually received"
cat "$EC_LOG"

say "9. the request's own state"
$EC status --request-id demo-001

say "10. the durable result (not ready yet, and it says so rather than guessing)"
$EC result --request-id demo-001 --json

say "done"
echo "store:  $WORK/store"
echo "state:  $WORK/store/REQUESTS/demo-001/STATE.json"
echo
echo "Next steps a real deployment takes from here:"
echo "  - the auditor answers with:  $EC review-result --request-id demo-001 \\"
echo "        --review-id A1 --kind COMPLETION --generation 1 --outcome PASS \\"
echo "        --report-file /path/to/report.md"
echo "  - and only then does the request reach VERIFIED with a published result."
