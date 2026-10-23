# Agent Engine agent entry point

For submitting work, read [the agent quickstart](docs/agent-quickstart.md).
Use `flows --json` to discover locally registered episode templates. Use
`episode prepare`, `episode validate`, then `episode launch`. Use `episode watch`
from a separate terminal to observe execution. Never use `episode drive` as a
launcher: it only advances stored state.

For modifying Agent Engine, inspect current source and relevant tests before editing.
Keep admission, execution, lifecycle and result decisions in their existing
owners. Agent Deck and OMA remain pinned, unmodified upstream dependencies.
Preserve unrelated work. Never fabricate qualification receipts, rewrite an
admitted request, clear an observation gap, or self-approve an implementation plan.
Do not run live model probes as part of ordinary tests. Use fixture processes.

Historical handovers record evidence, not today's operating instructions. The
quickstart, caller guide and source are the entry points. Record validation and
limitations honestly; a sample completion does not prove Forge/Android acceptance.
