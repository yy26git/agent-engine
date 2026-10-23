package control

// AP2 tests: transport selection, durable mixed-runtime state, and the journal
// durability contract.
//
// The claims under test are narrow and each has a specific way of being wrong:
// a transport that silently serves a role it should not; a selection that moves
// after admission; a legacy binding left unroutable; a journal that loses an
// observation a decision already consumed; a torn tail that discards good
// records; and two backends whose cursors get compared.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emjin/agent-engine/contract"
	"github.com/emjin/agent-engine/internal/runtime"
)

// --- transport selection ----------------------------------------------------

// TestAP2_TransportSelectionIsRoleConstrained asserts ACP cannot serve a review
// role.
//
// A reviewer's value is that it is an independent session whose output this layer
// can attribute. An ACP reviewer would be a process with a different lifecycle and
// receipt shape from the Agent Deck reviewers it is supposed to be independent
// of, so admitting one now would ship an independence claim nobody demonstrated.
func TestAP2_TransportSelectionIsRoleConstrained(t *testing.T) {
	f := profileFixture(t)
	f.cfg.Profiles["acp-worker"] = Profile{
		Name: "acp-worker", Role: RoleWorker, Tool: "mcode", Transport: TransportMiniMaxACP,
	}
	f.cfg.Profiles["acp-review"] = Profile{
		Name: "acp-review", Role: RoleReviewer, Tool: "mcode", Transport: TransportMiniMaxACP,
	}
	f.cfg.Profiles["acp-diag"] = Profile{
		Name: "acp-diag", Role: RoleDiagnostic, Tool: "mcode", Transport: TransportMiniMaxACP,
	}
	f.cfg.Profiles["acp-check"] = Profile{
		Name: "acp-check", Role: RoleCheckpoint, Tool: "mcode", Transport: TransportMiniMaxACP,
	}

	if _, err := f.cfg.ResolveProfile("acp-worker", RoleWorker); !errorsIs(err, ErrConfinementUnproven) {
		t.Fatalf("the supported worker role must still refuse its unqualified process boundary: %v", err)
	}
	for _, tc := range []struct {
		name string
		prof string
		role ProfileRole
	}{
		{"reviewer", "acp-review", RoleReviewer},
		{"diagnostic", "acp-diag", RoleDiagnostic},
		{"checkpoint", "acp-check", RoleCheckpoint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.cfg.ResolveProfile(tc.prof, tc.role); !errorsIs(err, ErrTransportUnsupported) {
				t.Fatalf("ACP served the %s role: %v", tc.name, err)
			}
		})
	}
}

// TestAP2_TransportDefaultsToLegacy asserts an unset transport keeps its old
// meaning, so no existing configuration changes behaviour.
func TestAP2_TransportDefaultsToLegacy(t *testing.T) {
	f := profileFixture(t)
	// The fixture's profiles declare no transport at all.
	p, err := f.cfg.ResolveProfile("impl", RoleWorker)
	if err != nil {
		t.Fatalf("a legacy profile no longer resolves: %v", err)
	}
	if got := p.TransportFor(); got != TransportAgentDeck {
		t.Errorf("transport = %q, want the legacy default %q", got, TransportAgentDeck)
	}
	if ref := p.Ref(); ref.Transport != TransportAgentDeck {
		t.Errorf("the pinned reference transport = %q, want %q", ref.Transport, TransportAgentDeck)
	}
}

// TestAP2_SelectionIsFrozenAtAdmission asserts a config edit cannot change how an
// in-flight request runs.
func TestAP2_SelectionIsFrozenAtAdmission(t *testing.T) {
	f := profileFixture(t)
	admitted, err := f.cfg.ResolveProfile("impl", RoleWorker)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	frozen := admitted.Ref()
	if frozen.Tool != "mcode" || frozen.Effort != "high" {
		t.Fatalf("the frozen reference lost its operator settings: %+v", frozen)
	}

	// The operator edits the profile underneath the running request.
	f.cfg.Profiles["impl"] = Profile{
		Name: "impl", Role: RoleWorker, Tool: "something-else", Model: "swapped",
		Transport: TransportMiniMaxACP,
	}
	if _, err := f.cfg.ResolveProfile("impl", RoleWorker); !errorsIs(err, ErrConfinementUnproven) {
		t.Fatalf("a new admission inherited the unqualified ACP edit: %v", err)
	}
	// The FROZEN reference is unaffected, which is the property that matters: an
	// admitted run keeps the identity it was admitted with.
	if frozen.Tool != "mcode" || frozen.Transport != TransportAgentDeck {
		t.Errorf("the frozen reference changed under a config edit: %+v", frozen)
	}
}

// --- migration --------------------------------------------------------------

// TestAP2_LegacyBindingsMapToAgentDeck asserts a v2 state becomes routable.
//
// A bare session ID cannot be routed: it is only unique inside the backend that
// minted it. Leaving it unqualified would mean every effect against a migrated
// request had to guess a transport.
func TestAP2_LegacyBindingsMapToAgentDeck(t *testing.T) {
	f := profileFixture(t)
	dir := filepath.Join(f.root, RequestsDirName, "forge-run-1842-repair")
	raw := `{"schema_version":2,"revision":4,"request_id":"forge-run-1842-repair",` +
		`"request_sha256":"` + strings.Repeat("c", 64) + `","stage":"WORKING",` +
		`"worker_session":"sess-worker-1","auditor_session":"sess-auditor-1"}`
	if err := os.WriteFile(filepath.Join(dir, StateFileName), []byte(raw), 0o600); err != nil {
		t.Fatalf("write v2 state: %v", err)
	}
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load migrated state: %v", err)
	}
	if st.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("schema version = %d, want %d", st.SchemaVersion, CurrentSchemaVersion)
	}
	for role, want := range map[ProfileRole]string{
		RoleWorker:   "sess-worker-1",
		RoleReviewer: "sess-auditor-1",
	} {
		b, ok := st.BackendBindings[role]
		if !ok {
			t.Fatalf("no backend binding for %s after migration", role)
		}
		if b.Backend != LegacyBackendAgentDeck {
			t.Errorf("%s bound to backend %q, want %q", role, b.Backend, LegacyBackendAgentDeck)
		}
		if b.SessionID != want {
			t.Errorf("%s session = %q, want %q", role, b.SessionID, want)
		}
		if !b.Resolved {
			t.Errorf("%s binding is not marked resolved; a migrated binding names a real session", role)
		}
	}
	// Nothing was invented.
	if st.ConsumedJournalSeq != 0 {
		t.Errorf("migration invented a consumed journal position: %d", st.ConsumedJournalSeq)
	}
}

// TestAP2_UnknownEffectsStayUnknown asserts migration resolves nothing it cannot
// prove.
//
// An effect in flight when the state was written has an unknown delivery outcome.
// Marking it settled would let a retry become a duplicate write.
func TestAP2_UnknownEffectsStayUnknown(t *testing.T) {
	f := profileFixture(t)
	dir := filepath.Join(f.root, RequestsDirName, "forge-run-1842-repair")
	raw := `{"schema_version":2,"revision":4,"request_id":"forge-run-1842-repair",` +
		`"request_sha256":"` + strings.Repeat("c", 64) + `","stage":"WORKING",` +
		`"effects":[{"effect_id":"E1","session_id":"sess-worker-1","status":"PENDING"}]}`
	if err := os.WriteFile(filepath.Join(dir, StateFileName), []byte(raw), 0o600); err != nil {
		t.Fatalf("write v2 state: %v", err)
	}
	st, err := f.store.State("forge-run-1842-repair")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(st.Effects) != 1 {
		t.Fatalf("effects = %d, want the record preserved", len(st.Effects))
	}
	if !st.Effects[0].Unresolved() {
		t.Errorf("migration resolved an in-flight effect to %q; its outcome was unknown", st.Effects[0].Status)
	}
}

// TestAP2_OlderReaderRefusesNewSchema asserts the new schema cannot be
// reinterpreted by a build that predates it.
func TestAP2_OlderReaderRefusesNewSchema(t *testing.T) {
	f := profileFixture(t)
	dir := filepath.Join(f.root, RequestsDirName, "forge-run-1842-repair")
	raw := fmt.Sprintf(`{"schema_version":%d,"revision":1,"request_id":"forge-run-1842-repair",`, CurrentSchemaVersion+1) +
		`"request_sha256":"` + strings.Repeat("c", 64) + `","stage":"WORKING"}`
	if err := os.WriteFile(filepath.Join(dir, StateFileName), []byte(raw), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := f.store.State("forge-run-1842-repair"); err == nil {
		t.Error("a state from a newer schema was accepted; a downgrade must refuse rather than guess")
	} else if !strings.Contains(err.Error(), "schema version") {
		t.Errorf("the refusal did not name the schema version: %v", err)
	}
}

// TestAP2_CrossBackendIDCollisionIsDistinguishable asserts two backends producing
// the same session ID stay distinguishable.
func TestAP2_CrossBackendIDCollisionIsDistinguishable(t *testing.T) {
	a := SessionBinding{Backend: LegacyBackendAgentDeck, SessionID: "s1", Resolved: true}
	b := SessionBinding{Backend: "minimax-acp", SessionID: "s1", Resolved: true}
	if a == b {
		t.Fatal("two backends produced identical bindings for the same session id")
	}
	raw, err := json.Marshal(map[ProfileRole]SessionBinding{RoleWorker: a, RoleReviewer: b})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Count(string(raw), `"s1"`) != 2 {
		t.Errorf("the two bindings did not both round-trip: %s", raw)
	}
}

// --- journal contract -------------------------------------------------------

func testJournal(t *testing.T) (*runtime.Journal, string) {
	t.Helper()
	dir := t.TempDir()
	j, err := runtime.OpenJournal(dir, func() string { return "2026-10-09T00:00:00Z" })
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	return j, dir
}

// TestAP2_JournalReplaysWhatWasNotConsumed asserts the crash window is
// survivable.
//
// An observation whose decision was never committed must come back; one whose
// decision WAS committed must not be offered again.
func TestAP2_JournalReplaysWhatWasNotConsumed(t *testing.T) {
	j, _ := testJournal(t)
	for i, kind := range []string{"session.status", "session.turn", "session.finished"} {
		if _, err := j.Append(runtime.Event{
			Backend: "agent-deck", Kind: kind, SessionID: "s1", Cursor: uint64(i + 1),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	res, err := j.Replay()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(res.Records) != 3 {
		t.Fatalf("replayed %d records, want 3", len(res.Records))
	}
	if err := res.VerifyChain(); err != nil {
		t.Errorf("the sequence chain is broken: %v", err)
	}
	// A decision committed after record 1 consumed exactly that one.
	res.Consumed = 1
	if got := len(res.Unconsumed()); got != 2 {
		t.Errorf("unconsumed = %d, want 2 (records after the committed position)", got)
	}
	if res.Unconsumed()[0].Seq != 2 {
		t.Errorf("first unconsumed seq = %d, want 2", res.Unconsumed()[0].Seq)
	}
}

// TestAP2_JournalTornTailIsQuarantinedNotFatal asserts a crash mid-append does not
// discard the good records before it.
//
// A torn final line is the EXPECTED shape of a crash during append. Failing to
// open the journal because of it would throw away every observation that was
// written correctly.
func TestAP2_JournalTornTailIsQuarantinedNotFatal(t *testing.T) {
	j, dir := testJournal(t)
	for i := 1; i <= 2; i++ {
		if _, err := j.Append(runtime.Event{Backend: "agent-deck", Kind: "k", Cursor: uint64(i)}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// Simulate a torn final append.
	f, err := os.OpenFile(filepath.Join(dir, "journal.ndjson"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	f.WriteString(`{"seq":3,"source":{"backend":"agent-deck","posi`)
	f.Close()

	reopened, err := runtime.OpenJournal(dir, func() string { return "2026-10-09T00:00:00Z" })
	if err != nil {
		t.Fatalf("a torn journal could not be reopened: %v", err)
	}
	res, err := reopened.Replay()
	if err != nil {
		t.Fatalf("replay after a torn tail: %v", err)
	}
	if len(res.Records) != 2 {
		t.Errorf("a torn tail discarded good records: %d survived, want 2", len(res.Records))
	}
	if res.Quarantined == 0 {
		t.Error("the torn record was not quarantined; it must be visible, not silently dropped")
	}
	// And the sequence CONTINUES past the torn record rather than reusing it.
	next, err := reopened.Append(runtime.Event{Backend: "agent-deck", Kind: "after", Cursor: 9})
	if err != nil {
		t.Fatalf("append after a torn tail: %v", err)
	}
	if next.Seq != 3 {
		t.Errorf("the next sequence is %d; the torn record's position was reused", next.Seq)
	}
}

// TestAP2_JournalUnattributableRecordIsQuarantined asserts a record with no source
// is never replayed.
//
// Replaying an observation nobody can attribute applies a decision to content
// with no traceable origin, which is worse than losing it.
func TestAP2_JournalUnattributableRecordIsQuarantined(t *testing.T) {
	j, dir := testJournal(t)
	if _, err := j.Append(runtime.Event{Backend: "agent-deck", Kind: "good", Cursor: 1}); err != nil {
		t.Fatalf("append: %v", err)
	}
	f, _ := os.OpenFile(filepath.Join(dir, "journal.ndjson"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"seq":2,"source":{"backend":"","position":9},"kind":"orphan"}` + "\n")
	f.WriteString(`{"seq":0,"source":{"backend":"agent-deck","position":9},"kind":"noseq"}` + "\n")
	f.Close()

	res, err := j.Replay()
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(res.Records) != 1 || res.Records[0].Kind != "good" {
		t.Errorf("unattributable records were replayed: %+v", res.Records)
	}
	if res.Quarantined != 2 {
		t.Errorf("quarantined = %d, want both unattributable records", res.Quarantined)
	}
}

// TestAP2_JournalSequenceSurvivesRestart asserts a restart continues the sequence
// rather than restarting it, which would collide with pre-restart records.
func TestAP2_JournalSequenceSurvivesRestart(t *testing.T) {
	j, dir := testJournal(t)
	if _, err := j.Append(runtime.Event{Backend: "agent-deck", Kind: "a", Cursor: 1}); err != nil {
		t.Fatalf("append: %v", err)
	}
	reopened, err := runtime.OpenJournal(dir, func() string { return "2026-10-09T00:00:00Z" })
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	rec, err := reopened.Append(runtime.Event{Backend: "agent-deck", Kind: "b", Cursor: 2})
	if err != nil {
		t.Fatalf("append after reopen: %v", err)
	}
	if rec.Seq != 2 {
		t.Errorf("post-restart seq = %d, want 2; a restarted sequence collides with the pre-restart record", rec.Seq)
	}
	res, _ := reopened.Replay()
	if err := res.VerifyChain(); err != nil {
		t.Errorf("the chain broke across a restart: %v", err)
	}
}

// TestAP2_JournalDigestIsStable asserts the evidence digest is reproducible, so a
// decision's inputs can be proven rather than asserted.
func TestAP2_JournalDigestIsStable(t *testing.T) {
	j, _ := testJournal(t)
	for i := 1; i <= 3; i++ {
		j.Append(runtime.Event{Backend: "agent-deck", Kind: "k", Cursor: uint64(i)})
	}
	a, _ := j.Replay()
	b, _ := j.Replay()
	if a.Digest() != b.Digest() {
		t.Error("two replays of the same journal produced different digests")
	}
	if len(a.Digest()) != 64 {
		t.Errorf("digest length = %d, want 64 hex characters", len(a.Digest()))
	}
}

// TestAP2_OversizedRecordIsRefused asserts a transport payload cannot grow the
// journal without bound.
func TestAP2_OversizedRecordIsRefused(t *testing.T) {
	j, _ := testJournal(t)
	huge := make([]byte, runtime.MaxJournalRecordBytes+1024)
	for i := range huge {
		huge[i] = 'x'
	}
	if _, err := j.Append(runtime.Event{Backend: "agent-deck", Kind: "k", Payload: huge}); err == nil {
		t.Error("an oversized observation was appended")
	}
	// And the rejected append did not consume a sequence number.
	res, _ := j.Replay()
	if len(res.Records) != 0 {
		t.Errorf("a refused append left %d records", len(res.Records))
	}
}

// --- dry-run non-mutation ---------------------------------------------------

// TestAP2_DryRunLaunchesNothing asserts configuring a transport is not the same
// as using one.
func TestAP2_DryRunLaunchesNothing(t *testing.T) {
	f := profileFixture(t)
	f.cfg.Profiles["acp-worker"] = Profile{
		Name: "acp-worker", Role: RoleWorker, Tool: "mcode", Transport: TransportMiniMaxACP,
	}
	if _, err := f.cfg.ResolveProfile("acp-worker", RoleWorker); !errorsIs(err, ErrConfinementUnproven) {
		t.Fatalf("the unqualified ACP profile was not refused: %v", err)
	}
	// Resolving a transport must not have started anything. The engine
	// has no ACP backend wired, so a launch attempt cannot silently succeed.
	st, _ := f.store.State("forge-run-1842-repair")
	if len(st.BackendBindings) != 0 {
		t.Errorf("resolving a profile created bindings: %+v", st.BackendBindings)
	}
	if st.LaunchEpoch != 0 {
		t.Errorf("resolving a profile advanced the launch epoch to %d", st.LaunchEpoch)
	}
}

func errorsIs(err, target error) bool {
	return err != nil && strings.Contains(err.Error(), target.Error())
}

// contractUnused keeps the contract import available for future schema work.
var _ = contract.Version
