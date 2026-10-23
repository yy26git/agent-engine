package control

import "fmt"

// RequireQualifiedMiniMaxBoundary is shared by admission and the production
// launch paths, including requests admitted by an older binary. The current
// native candidate has no qualified termination domain: a detached child wrote
// after a reported clean teardown. A proof boolean, receipt assertion or read-only
// prompt cannot authorize that candidate. Qualification tools remain separate
// from production and may gather evidence without admitting a repair worker.
//
// Add an accepted mechanism here only with its receipt verifier AND the exact
// production launcher composition. Accepting evidence without applying the
// boundary would reopen the same launch gap.
func RequireQualifiedMiniMaxBoundary(p Profile) error {
	if p.TransportFor() != TransportMiniMaxACP {
		return nil
	}
	return fmt.Errorf("%w: MiniMax ACP profile %q has no qualified whole-process termination domain; "+
		"the native candidate permits detached writers after teardown. Production execution is unavailable "+
		"until R2 ownership and R3 launch composition pass; changing confinement_proven or model settings cannot qualify it",
		ErrConfinementUnproven, p.Name)
}
