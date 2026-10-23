package control

import (
	"errors"
	"strings"
	"testing"
)

func TestMiniMaxBoundary_ProfileRefusesUnqualifiedExecution(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		for _, proven := range []bool{false, true} {
			p := Profile{
				Name: "mini-worker", Role: RoleWorker, Tool: "mcode",
				Transport: TransportMiniMaxACP, ReadOnly: readOnly, Proven: proven,
				Confinement: "sandbox", ConfinementReceipt: "/asserted/PASSED.json",
				ConfinementReceiptSHA256: strings.Repeat("a", 64),
			}
			c := &Config{Profiles: map[string]Profile{p.Name: p}}
			_, err := c.ResolveProfile(p.Name, RoleWorker)
			if !errors.Is(err, ErrConfinementUnproven) || !strings.Contains(err.Error(), "termination domain") {
				t.Fatalf("read_only=%v proven=%v: expected the whole-process refusal, got %v", readOnly, proven, err)
			}
		}
	}
}

func TestMiniMaxBoundary_EpisodeRefusalCommitsNothing(t *testing.T) {
	f := episodeBoundFixture(t, map[ProfileRole]string{RoleWorker: "impl", RoleReviewer: "audit"})
	p := f.cfg.Profiles["impl"]
	p.Transport, p.Tool, p.Proven = TransportMiniMaxACP, "mcode", true
	f.cfg.Profiles["impl"] = p
	_, err := f.store.AdmitEpisode(episodeRequest(f, "EP-UNQUALIFIED-MINIMAX"))
	if !errors.Is(err, ErrConfinementUnproven) || !strings.Contains(err.Error(), "termination domain") {
		t.Fatalf("unqualified writable worker was not refused at episode admission: %v", err)
	}
	if _, err := f.store.State("EP-UNQUALIFIED-MINIMAX"); err == nil {
		t.Fatal("refused episode left an admitted request")
	}
}
