package control

// Episode role binding.
//
// An admitted episode is bound to the profiles its approved template names. That
// binding is what makes an episode runnable: every reader of ExecutionProfiles —
// activation, assessment, refresh, review routing — asks "which profile fills
// this role", and an episode with an empty map has no answer for any of them, so
// the phases that depend on a role quietly have nothing to run.
//
// Engineering bindings go through ResolveProfile, the authority for
// "can this operator-approved profile serve this role". Reading cfg.Profiles
// directly would be shorter and would be wrong: it skips every refusal that
// function exists to make — an unapproved name, a profile approved for a
// different role, a transport that cannot serve the role, and a read-only
// profile whose confinement nobody has demonstrated. An episode binds a role for
// the whole of its run, so a refusal skipped here is a refusal that never
// happens at all; the launch path is not going to catch it later.
//
// Data-only report templates use ValidateReportProfile instead: native Cursor
// ask mode, a fixed launcher/model and an empty temporary working directory.
// This does not assert OS confinement or confer independent audit authority.
//
// Nothing here substitutes. A template that names a role this operator cannot
// fill is refused, because running the episode with a default profile, or with
// the role left unbound, would run a shape nobody approved while STATE.json
// claimed the approved one.

import (
	"fmt"
	"sort"

	"github.com/emjin/agent-engine/contract"
)

// episodeRoleProfiles resolves every role an approved template names into the
// durable identity that role will execute under.
//
// It is ALL OR NOTHING. The returned map is complete or the episode is refused;
// a partially-bound set is never returned, because a caller that commits it
// would record roles as approved that were never resolved, and the next reader
// of STATE.json could not tell a bound role from a missing one.
//
// Roles are resolved in sorted order. Go's map iteration order must not decide
// WHICH refusal a caller sees: a template whose reviewer and worker are both
// unresolvable has to name the same role every run, or a flaky refusal reads as
// a configuration that changes under the operator.
func (s *Store) episodeRoleProfiles(template EpisodeTemplate, workspace string) (map[ProfileRole]ProfileRef, error) {
	return s.episodeRoleProfilesForPilot(template, workspace, nil)
}

func (s *Store) episodeRoleProfilesForPilot(template EpisodeTemplate, workspace string, pilot *LocalPilotGrant) (map[ProfileRole]ProfileRef, error) {
	if len(template.Roles) == 0 {
		// Registered templates always name a reviewer, so this is unreachable
		// through Lookup. It is refused anyway rather than producing an empty
		// binding, because an episode committed with zero bound roles looks
		// exactly like an episode whose binding was forgotten.
		return nil, fmt.Errorf("%w: template %q binds no roles at all; there is no episode to run under it",
			contract.ErrEpisodeInvalid, template.Name)
	}
	roles := make([]string, 0, len(template.Roles))
	for role := range template.Roles {
		roles = append(roles, string(role))
	}
	sort.Strings(roles)

	out := make(map[ProfileRole]ProfileRef, len(roles))
	for _, raw := range roles {
		role := ProfileRole(raw)
		name := template.Roles[role]
		if template.ReportOnly {
			p, ok := s.cfg.Profiles[name]
			if !ok {
				return nil, fmt.Errorf("unknown report profile %q", name)
			}
			if err := ValidateReportProfile(p); err != nil {
				return nil, err
			}
			out[role] = p.Ref()
			continue
		}
		p, err := s.cfg.resolveProfile(name, role, pilot != nil && role == RoleWorker && pilot.WorkerProfile == name)
		if err != nil {
			// Both sentinels are wrapped on purpose. ErrEpisodeInvalid says the
			// episode cannot be admitted as asked; the inner error says WHICH
			// refusal stopped it, which is the part an operator has to fix.
			return nil, fmt.Errorf("%w: template %q binds the %s role to profile %q, which cannot serve it here: %w",
				contract.ErrEpisodeInvalid, template.Name, role, name, err)
		}
		if (role == RoleReviewer || role == RoleInvestigat) && (!p.ReadOnly || !p.Proven) {
			return nil, fmt.Errorf("%w: template %q requires an independent read-only %s, but profile %q has no demonstrated read-only boundary: %w", contract.ErrEpisodeInvalid, template.Name, role, p.Name, ErrConfinementUnproven)
		}
		if p.ReadOnly && p.Tool == "codex" {
			if _, err := s.cfg.VerifyConfinementReceipt(p, workspace); err != nil {
				return nil, fmt.Errorf("%w: template %q %s qualification: %w", contract.ErrEpisodeInvalid, template.Name, role, err)
			}
		}
		out[role] = p.Ref()
	}
	return out, nil
}
