package control

// S1 migration tests.
//
// The claim under test is narrow and unforgiving: ownership moves only when the
// legacy owner is provably unable to act, and it never moves on an approval
// nobody can attribute. Each test isolates one way that claim fails — an
// supervisor still running, one unknown send, a worker that is stopped but not
// quiescent, a legacy "verified" record, and a state file that cannot be read.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func migFixture(t *testing.T) *fixture {
	t.Helper()
	return boundRequest(t, newFixture(t))
}

// TestMigration_LegacyStoppedBeforeTransfer asserts a transfer is refused while
// the legacy owner could still be running.
func TestMigration_LegacyStoppedBeforeTransfer(t *testing.T) {
	f := migFixture(t)
	_, err := f.store.MigrateOwnership("forge-run-1842-repair", MigrationRequest{
		Source:                   "legacy-agent-deck-workflow",
		LegacySupervisorsStopped: false,
		Now:                      f0(),
	})
	if err == nil {
		t.Fatal("ownership transferred while the legacy supervisors were still able to act")
	}
	if !errors.Is(err, ErrTransferBlocked) {
		t.Errorf("refusal = %v, want ErrTransferBlocked", err)
	}
	if !strings.Contains(err.Error(), "concurrent-writer") {
		t.Errorf("the refusal did not explain the risk: %v", err)
	}
	// Nothing may have been written.
	st, serr := f.store.State("forge-run-1842-repair")
	if serr != nil {
		t.Fatalf("state: %v", serr)
	}
	if len(st.Transfers) != 0 {
		t.Errorf("a refused transfer left %d transfer records", len(st.Transfers))
	}
	if len(st.RetiredOwners) != 0 {
		t.Errorf("a refused transfer retired an owner: %v", st.RetiredOwners)
	}
}

// TestMigration_UnknownEffectBlocksTransfer asserts one unprovable legacy effect
// blocks the whole transfer.
func TestMigration_UnknownEffectBlocksTransfer(t *testing.T) {
	cases := []struct {
		name   string
		effect LegacyEffect
	}{
		{"a send issued with no receipt", LegacyEffect{Kind: "send", Identity: "send-7"}},
		{"a worker stopped but not quiescent", LegacyEffect{Kind: "worker", Identity: "sess-legacy-1", Settled: true}},
		{"an approval of unknown lineage", LegacyEffect{Kind: "approval", Identity: "A-3", Settled: true}},
		{"an effect of an unrecognised kind", LegacyEffect{Kind: "mystery", Identity: "x", Settled: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := migFixture(t)
			_, err := f.store.MigrateOwnership("forge-run-1842-repair", MigrationRequest{
				Source:                   "legacy-agent-deck-workflow",
				LegacySupervisorsStopped: true,
				Effects:                  []LegacyEffect{tc.effect},
				Now:                      f0(),
			})
			if err == nil {
				t.Fatalf("ownership transferred despite %s", tc.name)
			}
			if !errors.Is(err, ErrUnknownEffect) {
				t.Errorf("refusal = %v, want ErrUnknownEffect", err)
			}
			if !strings.Contains(err.Error(), tc.effect.Identity) {
				t.Errorf("the refusal did not name the unresolved effect %q: %v", tc.effect.Identity, err)
			}
		})
	}
}

// TestMigration_LegacyVerifiedIsNotNewPass asserts a legacy approval is preserved
// as history and can never be promoted.
func TestMigration_LegacyVerifiedIsNotNewPass(t *testing.T) {
	f := migFixture(t)
	rec, err := f.store.MigrateOwnership("forge-run-1842-repair", MigrationRequest{
		Source:                   "legacy-agent-deck-workflow",
		LegacySupervisorsStopped: true,
		Approved:                 []string{"legacy-A-1"},
		Now:                      f0(),
	})
	if err != nil {
		t.Fatalf("a clean transfer was refused: %v", err)
	}
	if rec.AssuranceLevel != "unverified" {
		t.Errorf("assurance = %q, want unverified: the legacy engine never established who approved",
			rec.AssuranceLevel)
	}
	if len(rec.Approved) != 1 || rec.Approved[0] != "legacy-A-1" {
		t.Errorf("the legacy approval was not preserved as history: %v", rec.Approved)
	}
	// Promotion is refused, permanently and explicitly.
	if err := PromoteLegacyPass("legacy-A-1"); err == nil {
		t.Fatal("a legacy approval was promoted to a PASS")
	}
	st, serr := f.store.State("forge-run-1842-repair")
	if serr != nil {
		t.Fatalf("state: %v", serr)
	}
	if st.LegacyAssurance != "unverified" {
		t.Errorf("state assurance = %q, want unverified", st.LegacyAssurance)
	}
	// A fresh independent review is owed, not inherited.
	if st.ActiveReview != nil {
		t.Error("the transfer opened a review on the legacy engine's behalf")
	}
	if st.Stage.Terminal() {
		t.Errorf("the transfer made the request terminal as %s", st.Stage)
	}
}

// TestMigration_StateExportPreservesCountersAndMemory asserts the transfer records
// the state it inspected, byte for byte.
func TestMigration_StateExportPreservesCountersAndMemory(t *testing.T) {
	f := migFixture(t)
	// Give the request some history worth preserving.
	if _, err := f.store.UpdateCommitted("forge-run-1842-repair", func(s *State) error {
		s.RepairCycles = 2
		s.DiagnosticRounds = 1
		if _, err := s.OpenQuestion("which approach?", "two viable", "implementation", f0()); err != nil {
			return err
		}
		s.DecisionAuthorityRole = "operator"
		_, err := s.RecordDecision("use the adapter", "it is the only public seam", "", "operator",
			[]string{"ev-1"}, f0())
		return err
	}); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	before, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	rec, err := f.store.MigrateOwnership("forge-run-1842-repair", MigrationRequest{
		Source:                   "legacy-agent-deck-workflow",
		LegacySupervisorsStopped: true,
		Now:                      f0(),
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if rec.OriginalStateSHA256 == "" {
		t.Fatal("the transfer recorded no digest of the state it inspected")
	}
	if rec.OriginalStateExport == "" {
		t.Fatal("the transfer preserved no original state")
	}
	// The export must exist and hash to what the record claims.
	exported := filepath.Join(f.root, rec.OriginalStateExport)
	data, rerr := os.ReadFile(exported)
	if rerr != nil {
		t.Fatalf("the preserved state is missing: %v", rerr)
	}
	if got := sha256Hex(data); got != rec.OriginalStateSHA256 {
		t.Errorf("the preserved state hashes to %s but the record claims %s", got, rec.OriginalStateSHA256)
	}
	// And the live state must still carry the counters and memory.
	after, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if after.RepairCycles != before.RepairCycles {
		t.Errorf("repair cycles = %d, want %d: the transfer reset a lifetime counter",
			after.RepairCycles, before.RepairCycles)
	}
	if after.DiagnosticRounds != before.DiagnosticRounds {
		t.Errorf("diagnostic rounds = %d, want %d", after.DiagnosticRounds, before.DiagnosticRounds)
	}
	if len(after.Questions) != len(before.Questions) || len(after.Decisions) != len(before.Decisions) {
		t.Errorf("durable memory changed across the transfer: %d questions / %d decisions became %d / %d",
			len(before.Questions), len(before.Decisions), len(after.Questions), len(after.Decisions))
	}
	if len(after.RetiredOwners) != 1 || after.RetiredOwners[0] != "legacy-agent-deck-workflow" {
		t.Errorf("retired owners = %v, want the legacy draft recorded once", after.RetiredOwners)
	}
	if len(after.Transfers) != 1 {
		t.Errorf("transfers = %d, want exactly one record", len(after.Transfers))
	}
}

// TestMigration_ExportFailureLeavesOwnershipUnchanged asserts a transfer whose
// bytes cannot be preserved does not half-happen.
func TestMigration_ExportFailureLeavesOwnershipUnchanged(t *testing.T) {
	f := migFixture(t)
	// Remove the state file so the pre-transfer bytes cannot be read. The
	// transfer must refuse rather than record a digest of nothing.
	statePath := filepath.Join(f.root, "REQUESTS", "forge-run-1842-repair", StateFileName)
	if err := os.Remove(statePath); err != nil {
		t.Fatalf("remove state: %v", err)
	}
	_, err := f.store.MigrateOwnership("forge-run-1842-repair", MigrationRequest{
		Source:                   "legacy-agent-deck-workflow",
		LegacySupervisorsStopped: true,
		Now:                      f0(),
	})
	if err == nil {
		t.Fatal("a transfer completed with no readable pre-transfer state")
	}
	if !strings.Contains(err.Error(), "known bytes") {
		t.Errorf("the refusal did not explain the missing evidence: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(f.root, "REQUESTS", "forge-run-1842-repair", "MIGRATION-ORIGINAL-STATE.json")); serr == nil {
		t.Error("a failed transfer still wrote an export")
	}
}

// TestMigration_TerminalRequestIsNotTransferred asserts a finished outcome is
// history rather than something to take ownership of.
func TestMigration_TerminalRequestIsNotTransferred(t *testing.T) {
	f := completionFixture(t)
	reviewID := auditedRequest(t, f)
	if _, err := submitCompletionPass(t, f, reviewID, judgedCompletionBody(t, f)); err != nil {
		t.Fatalf("complete the request: %v", err)
	}
	_, err := f.store.MigrateOwnership("forge-run-1842-repair", MigrationRequest{
		Source:                   "legacy-agent-deck-workflow",
		LegacySupervisorsStopped: true,
		Now:                      f0(),
	})
	if err == nil {
		t.Fatal("a terminal request accepted an ownership transfer")
	}
	if !errors.Is(err, ErrTransferBlocked) {
		t.Errorf("refusal = %v, want ErrTransferBlocked", err)
	}
}

// currentReviewID reads the open review's ID.
func currentReviewID(t *testing.T, f *fixture) string {
	t.Helper()
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.ActiveReview == nil {
		t.Fatal("no review is open")
	}
	return st.ActiveReview.ReviewID
}

// TestMigration_TimeIsInjected proves the record is deterministic under test
// rather than depending on the wall clock.
func TestMigration_TimeIsInjected(t *testing.T) {
	f := migFixture(t)
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	rec, err := f.store.MigrateOwnership("forge-run-1842-repair", MigrationRequest{
		Source:                   "legacy-agent-deck-workflow",
		LegacySupervisorsStopped: true,
		Now:                      at,
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if rec.TransferredAt != at.Format(time.RFC3339) {
		t.Errorf("transferred at %q, want the injected %q", rec.TransferredAt, at.Format(time.RFC3339))
	}
}
