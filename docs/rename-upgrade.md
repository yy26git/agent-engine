# Agent Engine naming and upgrade

The project now uses one name throughout the source checkout:

- Executable and command directory: `agent-engine` and `cmd/agent-engine`.
- Go module: `github.com/emjin/agent-engine` (a source identity, not a claim that a public repository has been published).
- CLI/API product identifier: `agent-engine`.
- Environment settings and fixture opt-ins: `AGENT_ENGINE_` prefixes.
- New generated session labels and executor team names use the current name.

## Updating an existing installation

Stop or finish active supervisors before replacing their binaries. Back up the store and configuration. Rebuild using `scripts/build-agent-engine.sh`, then check `version --json` and `capabilities --json`.

Update launcher paths, environment settings, imports and client product-identity checks together. The previous executable, module import path, product identifier and environment names have no aliases. External applications maintained in other repositories need their own integration update; they are not renamed by this checkout.

The JSON state schema remains 7. This naming change does not rewrite admitted requests, stored labels, execution profiles, digests, authorizations, credentials or qualification receipts. Existing stored process/session identities must continue to be recovered from their recorded values. Any changed launcher/configuration still needs the normal admission and qualification checks. Do not edit retained requests to make a changed deployment look identical.

## Historical records

Source documentation and reference paths have been normalized to the current project name. Dated manifests and test reports remain historical records, not fresh verification of the renamed source or evidence that renamed external paths exist. Their recorded hashes are not regenerated to manufacture a new qualification. Use fresh build/test results for this revision.

The rename applies to the working source tree and rebuilt artifacts. Git commit history and independently stored operator data retain their original bytes; rewriting those would change historical evidence and commit identities.

## Verification — 2026-10-23

The rebuilt CLI reports product `agent-engine`. A case-insensitive scan of working-tree file contents and paths, including generated artifacts and excluding Git metadata, found no previous project-name references. Build, vet, the complete fixture test suite and full race suite passed. A focused race regression also proves that an activation recorded under a previous naming prefix is reconciled using its original ledger identity and session, without creating a replacement. Live model probes were disabled.
