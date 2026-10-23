# Maintain Agent Deck and OMA as upstream dependencies

Agent Engine owns engineering policy and its public-boundary adapters. Agent Deck owns
sessions and OMA owns outer scheduling. Production uses unmodified, explicitly
approved upstream artifacts. This architecture requires no maintained fork of
either dependency. The locally modified Agent Deck checkout is a preserved
engineering draft; do not build the production runtime from that checkout.

## Current approvals

| Dependency | Source approval | Artifact approval |
|---|---|---|
| Agent Deck | `e19e23dac840016f926f35398706bfc398d96c9d` | `runtime-lock.json`, platform binary SHA-256 and clean-source build receipt |
| OMA | `75a67581c98df3bce3c90035dcaf445a989155d2`, package 1.21.1 | Operator configuration plus `.oma-runtime/oma/1.21.1/BUILD-RECEIPT.json` and launcher digest |

The Agent Engine dependency maintainer prepares evidence and an operator approves
adoption. Until an actual person is assigned, that ownership is an unresolved
deployment responsibility; no GitHub owner or permission is inferred.

## Report and contribute upstream

1. Reproduce a suspected dependency defect using a clean checkout of the pinned
   upstream revision and a minimal public-interface example.
2. Distinguish an adapter/configuration error from an upstream defect. In
   particular, OMA's process discriminator is `backend.kind = "process"`.
   Correctly translated process agents run without a provider model call; a
   required model-shaped field is not evidence that an LLM was invoked.
3. Prepare an upstream issue or patch with the expected/actual behavior, exact
   revision, minimal regression test and compatibility impact. Use the upstream
   project's contribution rules. No message or PR has been submitted by this
   implementation.
4. Track upstream acceptance and the commit/release containing the fix. An
   accepted proposal or merged PR does not change the production pin.
5. Adopt the accepted upstream revision only through the upgrade gate below.
   Pending an upstream fix, keep the affected capability blocked or implement
   a reviewed Agent Engine adapter using an existing public seam. Do not ship a local
   runtime patch as an assumed upstream capability.

## Upgrade gate

1. Record candidate repository, release, exact commit, public CLI/protocol changes
   and contribution/fix references. Review licensing and installation changes.
2. Build/download into a separate versioned candidate location. Record clean
   source provenance, platform, build tools, artifact hashes and build receipt.
   Keep the current artifact and approval intact. For OMA, retain the referenced
   built package and Node runtime too: hashing only its launcher does not freeze
   all transitive executable bytes.
3. Compare upstream tests against the current clean baseline. Preserve known
   failure signatures such as G13; retries must not hide a new regression.
4. Run Agent Engine's fresh full suite, build, vet and affected race checks. Exercise
   the candidate's real public interfaces in an isolated, credential-free
   harness: JSON identity/output, inventory, persisted wrapper/model selection,
   NDJSON retention/cursors, queued-send ambiguity, stop/inspect, OMA dependency
   order/failure/cancellation and process-backend credential independence.
5. For changed provider/capability behavior, complete the actual selected CLI's
   bounded live model, status, confinement and cleanup tests. Model receipts
   must match the chosen stage profiles. Fixtures alone cannot close that gate.
6. Produce a reviewable evidence manifest and explicit compatibility decision.
   Only after operator approval update the lock/configuration and receipt
   together. Reject a candidate when the public contract is incompatible.
7. Keep admitted work tied to its original runtime and profile identities.
   Finish/cancel/quiesce affected runs before switching deployments; do not
   silently reinterpret an admitted request under a new runtime or schema.
8. Smoke-test the selected approval and keep a rollback record. Rollback restores
   the prior artifacts and matching configuration; preserve state, results and
   evidence. Do not run older code against new state fields without proving
   compatibility or taking the documented checksummed state export.

An upstream release is a candidate for review, never an automatic upgrade.
The current pins were retained by the 2026-10-08 selection implementation.
