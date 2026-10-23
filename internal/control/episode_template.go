package control

// Episode templates: the operator-approved shape of one episode.
//
// An episode spans investigation -> report review -> plan -> plan acceptance ->
// implement -> verify -> completion audit. A template says which of those phases
// this deployment actually uses, in what order, who fills each role, which checks
// must run, and which backwards moves are permitted.
//
// The load-bearing property is that a template is a TEMPLATE, not a program. A
// request references a template BY NAME and nothing else: no shell, no argv, no
// condition language and no mutable control configuration may arrive from a
// caller. That is the same separation the profile registry enforces for
// executables, applied to episode shape — a request that could carry its own
// template would be carrying its own grant, and the audit that judges the grant
// would have been chosen by the grantee.
//
// Everything here therefore refuses rather than repairs, and the registry is
// frozen on the way in: a template is copied on Register and on Lookup, so
// whatever was approved cannot afterwards be edited by whoever holds a copy.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/emjin/agent-engine/contract"
)

// EpisodeTemplate is a locally approved episode shape.
type EpisodeTemplate struct {
	Name string
	// ReportOnly admits one data-only producer with no engineering approval authority.
	ReportOnly bool
	// Phases this template may use, in the order they occur. A phase outside
	// this list can never be entered under this template.
	Phases []contract.EpisodePhase
	// Roles available, by role. A template that names no reviewer cannot be
	// admitted at all, because nothing could ever audit it.
	Roles map[ProfileRole]string // role -> locally approved profile name
	// Checks the template requires.
	RequiredChecks []string
	// Bounded return edges: from -> to. A phase move not in this map and not a
	// forward step in contract.PhaseOrder is refused.
	ReturnEdges map[contract.EpisodePhase][]contract.EpisodePhase
	// MaxRepairIterations bounds correction attempts inside the episode.
	MaxRepairIterations int
	// RepairEnabled explicitly opts into rejection/revision loops. Any legacy
	// template that already declares return_edges is also treated as opted in;
	// the edges themselves are an explicit bounded grant. A template with
	// neither this flag nor return edges is fail-stop on rejection.
	RepairEnabled bool
	// MaxWallSeconds overrides the one-hour default only when explicitly approved locally.
	// Admission still caps this allowance by the immutable pilot grant.
	MaxWallSeconds int
	// ReadOnly reports whether the template may change code at all.
	ReadOnly bool
	// Nonempty requires a new per-episode branch prepared before admission.
	// Empty preserves legacy templates; new repair deployments should configure it.
	RepairBranchPrefix string
}

// EpisodeTemplateRegistry is the set of locally approved episode templates.
//
// The zero value is ready to use, so a registry does not have to be built
// through a constructor before it can be filled.
type EpisodeTemplateRegistry struct {
	templates map[string]EpisodeTemplate
}

// NewEpisodeTemplateRegistry returns an empty registry.
func NewEpisodeTemplateRegistry() EpisodeTemplateRegistry {
	return EpisodeTemplateRegistry{templates: map[string]EpisodeTemplate{}}
}

// Register admits a locally approved template.
//
// It REFUSES rather than repairs, because every repair here would be the
// control plane quietly substituting its own idea of what an episode is:
//
//   - an empty name, or an empty phase list, since a request that references a
//     template by name cannot reference either;
//   - a template with no reviewer role, because nothing could ever audit it;
//   - a non-positive MaxRepairIterations, because an unbounded repair budget is
//     an episode that can loop forever while still calling itself progress;
//   - a duplicate name, because overwrite is how an approved policy silently
//     becomes a different one;
//   - a return edge naming a phase the template does not declare;
//   - a "return" edge that actually moves FORWARD, which is a modelling error
//     rather than a lenient permission.
//   - a repair-enabled template whose declared rejection phases do not each
//     have their explicit bounded return route.
//
// Three further refusals are this file's own judgement, in the same fail-closed
// direction as the above. Each is a typo or a mistake that would otherwise
// become an inert permission nobody notices:
//
//   - a phase listed TWICE. This is deliberately NOT treated like a duplicate
//     name: the phase list is ORDERED ("in the order they occur"), so a repeated
//     phase makes the shape ambiguous — there is no longer one answer to which
//     occurrence is meant — and no harmless reading of it survives.
//   - a phase string that is not in contract.PhaseOrder. Such a phase can never
//     be advanced to, because Advances refuses unknown phases, so it would be a
//     declared step that is permanently unreachable.
//   - an empty profile name for a declared role, which is a role that can never
//     be filled. Register additionally refuses a template whose worker and
//     reviewer resolve to the SAME profile: the whole value of a reviewer is
//     that it is independent of the implementer, and a template that names one
//     session for both makes that independence decorative from the first phase.
func (r *EpisodeTemplateRegistry) Register(t EpisodeTemplate) error {
	t.Name = strings.TrimSpace(t.Name)
	field := "episode_templates"
	if t.Name != "" {
		field = "episode_templates." + t.Name
	}
	if err := t.validateAt(field); err != nil {
		return err
	}
	if r.templates == nil {
		r.templates = map[string]EpisodeTemplate{}
	}
	if _, exists := r.templates[t.Name]; exists {
		return cfgErr(field, "a template with this name is already registered; overwrite is not allowed, because an approved policy must not be quietly replaced by a second one that happens to share its name")
	}
	r.templates[t.Name] = t.frozen()
	return nil
}

// Lookup returns the template approved under name.
//
// The copy is a defensive one: the registry holds the approved policy, and a
// caller holding a reference into it would be able to edit the grant after
// admission.
func (r EpisodeTemplateRegistry) Lookup(name string) (EpisodeTemplate, bool) {
	t, ok := r.templates[strings.TrimSpace(name)]
	if !ok {
		return EpisodeTemplate{}, false
	}
	return t.frozen(), true
}

// Names lists the registered template names, sorted.
//
// Sorted rather than map-ordered so operator output and test expectations do not
// depend on Go's map iteration order.
func (r EpisodeTemplateRegistry) Names() []string {
	out := make([]string, 0, len(r.templates))
	for name := range r.templates {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// AllowsPhase reports whether this template may ever enter p.
func (t EpisodeTemplate) AllowsPhase(p contract.EpisodePhase) bool {
	for _, declared := range t.Phases {
		if declared == p {
			return true
		}
	}
	return false
}

// AllowsMove reports whether the template permits the phase move from -> to.
//
// True when contract.Advances says the move is a forward step, OR when the edge
// is declared in ReturnEdges. Everything else — a sideways move, a backwards
// move nobody declared, a move to or from a phase outside the template — is
// refused, which is what keeps an episode from appearing to progress by
// wandering.
//
// Phase admission is a separate question: a caller that only asks this method
// has not checked that both phases are in the template at all.
func (t EpisodeTemplate) AllowsMove(from, to contract.EpisodePhase) bool {
	if contract.Advances(from, to) {
		return true
	}
	for _, dst := range t.ReturnEdges[from] {
		if dst == to {
			return true
		}
	}
	return false
}

// RequiresReviewer reports whether the template names a reviewer role.
//
// It is always true for a registered template; it exists so the admission path
// can ask the question of a candidate template before registering it, and so a
// zero-value template cannot be mistaken for an approved one.
func (t EpisodeTemplate) RequiresReviewer() bool {
	_, ok := t.Roles[RoleReviewer]
	return ok
}

// ProfileFor returns the locally approved profile name bound to a role.
func (t EpisodeTemplate) ProfileFor(role ProfileRole) (string, bool) {
	name, ok := t.Roles[role]
	if !ok {
		return "", false
	}
	if strings.TrimSpace(name) == "" {
		return "", false
	}
	return name, true
}

// frozen copies every mutable field, so an approved template cannot be edited
// after the fact through a slice or map the caller still holds.
func (t EpisodeTemplate) frozen() EpisodeTemplate {
	out := t
	out.Phases = append([]contract.EpisodePhase(nil), t.Phases...)
	out.RequiredChecks = append([]string(nil), t.RequiredChecks...)
	if t.Roles != nil {
		out.Roles = make(map[ProfileRole]string, len(t.Roles))
		for role, name := range t.Roles {
			out.Roles[role] = name
		}
	}
	if t.ReturnEdges != nil {
		out.ReturnEdges = make(map[contract.EpisodePhase][]contract.EpisodePhase, len(t.ReturnEdges))
		for from, tos := range t.ReturnEdges {
			out.ReturnEdges[from] = append([]contract.EpisodePhase(nil), tos...)
		}
	}
	return out
}

// validateAt applies every registration refusal, reporting under field.
func (t EpisodeTemplate) validateAt(field string) error {
	if t.RepairBranchPrefix != "" && (t.ReadOnly || !strings.HasSuffix(t.RepairBranchPrefix, "/") || strings.HasPrefix(t.RepairBranchPrefix, "-") || strings.ContainsAny(t.RepairBranchPrefix, " \t\r\n~^:?*[\\") || strings.Contains(t.RepairBranchPrefix, "..")) {
		return cfgErr(field+".repair_branch_prefix", "must be a Git branch prefix ending in / on a writable template")
	}
	if t.Name == "" {
		return cfgErr(field, "a template name is empty; a request references a template by name, so an unnamed one can never be referenced")
	}
	if len(t.Phases) == 0 {
		return cfgErr(field+".phases", "at least one phase is required; a template with no phases admits an episode that cannot begin")
	}
	seenPhase := map[contract.EpisodePhase]bool{}
	for _, p := range t.Phases {
		if seenPhase[p] {
			// Not a duplicate-NAME case. The list is ordered, so a repeated phase
			// leaves the shape with no single reading.
			return cfgErr(field+".phases", "phase %s appears twice; the phase list is ordered, so a repeated phase has no unambiguous meaning", p)
		}
		seenPhase[p] = true
		if !knownEpisodePhase(p) {
			// An unknown phase can never be advanced to, so it would be a step
			// the episode declares and can never reach.
			return cfgErr(field+".phases", "%q is not an episode phase", p)
		}
	}
	if t.MaxWallSeconds < 0 {
		return cfgErr(field+".max_wall_seconds", "must be nonnegative; zero retains the default")
	}
	if t.MaxRepairIterations <= 0 {
		return cfgErr(field+".max_repair_iterations", "must be positive, got %d; an unbounded repair budget is an episode that can loop forever while still calling itself progress", t.MaxRepairIterations)
	}
	if len(t.Roles) == 0 {
		return cfgErr(field+".roles", "no roles are declared")
	}
	for _, id := range t.RequiredChecks {
		if strings.TrimSpace(id) == "" {
			return cfgErr(field+".required_checks", "a required check id is empty; a nameless check can never run and would report a pass it did not perform")
		}
	}
	if err := t.validateRoles(field); err != nil {
		return err
	}
	return t.validateReturnEdges(field, seenPhase)
}

// validateRoles refuses a role set that could not produce a real audit.
func (t EpisodeTemplate) validateRoles(field string) error {
	names := make([]string, 0, len(t.Roles))
	for role := range t.Roles {
		names = append(names, string(role))
	}
	sort.Strings(names)
	for _, raw := range names {
		role := ProfileRole(raw)
		switch role {
		case RoleWorker, RoleReviewer, RoleCheckpoint, RoleDiagnostic, RoleInvestigat:
		default:
			// Refused rather than ignored, matching loadProfiles: a role key that
			// is a typo would otherwise silently disable the role it names.
			return cfgErr(field+".roles", "%q is not a role; must be one of worker, reviewer, checkpoint, diagnostic, investigation", role)
		}
		if strings.TrimSpace(t.Roles[role]) == "" {
			return cfgErr(field+".roles", "role %s names no profile; a role bound to nothing can never be filled", role)
		}
	}
	if t.ReportOnly {
		if !t.ReadOnly || len(t.Phases) != 1 || t.Phases[0] != contract.PhaseReport || len(t.Roles) != 1 || t.Roles[RoleInvestigat] == "" || t.repairRoutingEnabled() || len(t.RequiredChecks) > 0 {
			return cfgErr(field, "report_only requires read_only, REPORT only, one investigation role, no checks or repair routes")
		}
		return nil
	}
	if !t.RequiresReviewer() {
		return cfgErr(field+".roles", "no reviewer role; a template that names no reviewer admits an episode nothing could ever audit")
	}
	if w, ok := t.ProfileFor(RoleWorker); ok {
		if rv, ok := t.ProfileFor(RoleReviewer); ok && strings.TrimSpace(w) == strings.TrimSpace(rv) {
			return cfgErr(field+".roles", "worker and reviewer both resolve to profile %q; a reviewer is only independent if it is not the implementer", rv)
		}
	}
	return nil
}

// validateReturnEdges refuses edges that do not describe a bounded backwards move.
func (t EpisodeTemplate) validateReturnEdges(field string, declared map[contract.EpisodePhase]bool) error {
	froms := make([]contract.EpisodePhase, 0, len(t.ReturnEdges))
	for from := range t.ReturnEdges {
		froms = append(froms, from)
	}
	sort.Slice(froms, func(i, j int) bool { return froms[i] < froms[j] })
	for _, from := range froms {
		if !declared[from] {
			return cfgErr(field+".return_edges", "%s is not a phase this template declares", from)
		}
		for _, to := range t.ReturnEdges[from] {
			if !declared[to] {
				return cfgErr(field+".return_edges", "%s -> %s names a phase this template does not declare", from, to)
			}
			if from == to && from != contract.PhaseImplement {
				return cfgErr(field+".return_edges", "%s -> %s is a self edge; only an explicitly bounded implementation correction may re-enter its phase", from, to)
			}
			if contract.Advances(from, to) {
				// A "return" that moves forward is a modelling error. It would
				// also be harmless as a permission, which is exactly why it is
				// refused: the map is a record of REVISIONS, and an entry here
				// says the operator expects this episode to go backwards.
				return cfgErr(field+".return_edges", "%s -> %s moves forward in the normal progression; a return edge exists to record a revision, not to restate an ordinary step", from, to)
			}
		}
	}
	if t.RepairEnabled && len(t.ReturnEdges) == 0 {
		return cfgErr(field+".return_edges", "repair_enabled is true but no bounded rejection return routes are declared")
	}
	if t.repairRoutingEnabled() {
		// These are exactly the rejection paths that episodeReturn can take.
		// Require the complete set relevant to the phases this template declares;
		// a partial set would make one rejection loop while another unexpectedly
		// stops, despite the template opting into repair.
		required := []struct {
			from contract.EpisodePhase
			to   contract.EpisodePhase
		}{
			{contract.PhaseReportReview, contract.PhaseReport},
			{contract.PhaseImplement, contract.PhaseImplement},
			{contract.PhaseVerify, contract.PhaseImplement},
			{contract.PhaseCompletionReview, contract.PhaseImplement},
		}
		for _, edge := range required {
			if !declared[edge.from] {
				continue
			}
			if !declared[edge.to] {
				return cfgErr(field+".return_edges", "repair-enabled phase %s can be rejected, but its required return destination %s is not declared", edge.from, edge.to)
			}
			found := false
			for _, to := range t.ReturnEdges[edge.from] {
				if to == edge.to {
					found = true
					break
				}
			}
			if !found {
				return cfgErr(field+".return_edges", "repair-enabled phase %s requires the explicit bounded return edge %s=%s", edge.from, edge.from, edge.to)
			}
		}
	}
	return nil
}

// repairRoutingEnabled reports whether this template explicitly grants a
// rejection loop. A pre-existing return_edges list remains an explicit grant
// for backward compatibility; a new template may say repair_enabled=true to
// make the intent visible even while its required edge set is being authored.
func (t EpisodeTemplate) repairRoutingEnabled() bool {
	return t.RepairEnabled || len(t.ReturnEdges) > 0
}

// RejectionDisposition gives discovery clients the effective, backwards-
// compatible rejection policy without exposing implementation details.
func (t EpisodeTemplate) RejectionDisposition() string {
	if t.repairRoutingEnabled() {
		return "bounded_return_edges"
	}
	return "stop_blocked"
}

// knownEpisodePhase reports whether p is a phase this build understands.
func knownEpisodePhase(p contract.EpisodePhase) bool {
	for _, known := range contract.PhaseOrder {
		if known == p {
			return true
		}
	}
	return false
}

// loadEpisodeTemplates reads the operator-approved episode template registry
// from the `episode_templates` table of CONTROL.toml.
//
// The table is optional, mirroring `profiles`: a deployment that has not
// approved any episode shape keeps working for ordinary requests, and what it
// loses is the ability to admit an episode, which is a safer failure than
// inventing a default episode. It is NOT optional once it is present — a table
// that is present but empty, malformed, or names something that would not
// register is an error, because silently ignoring policy the operator wrote is
// how a config that "probably configured it right" happens.
//
// Shape, following the launch_env idiom for a map encoded as an array:
//
//	[episode_templates.standard_fix]
//	phases = ["REPORT", "REPORT_REVIEW", "PLAN", "PLAN_ACCEPT", "IMPLEMENT", "VERIFY", "COMPLETION_REVIEW"]
//	roles = ["worker=impl", "reviewer=audit"]
//	required_checks = ["build", "test"]
//	return_edges = ["REPORT_REVIEW=REPORT", "IMPLEMENT=IMPLEMENT", "VERIFY=IMPLEMENT", "COMPLETION_REVIEW=IMPLEMENT"]
//	max_repair_iterations = 2
//	read_only = "false"
//	repair_enabled = "true"
//
// A template with no return_edges remains a fail-stop workflow for compatibility:
// rejected work commits a durable stop marker and a BLOCKED result. Any legacy
// nonempty return_edges opts into repair and is checked for the full set needed by
// its declared rejection phases. No edge is inferred.
func loadEpisodeTemplates(doc map[string]any) (EpisodeTemplateRegistry, error) {
	raw, present := doc["episode_templates"]
	if !present {
		return NewEpisodeTemplateRegistry(), nil
	}
	entries, ok := raw.(map[string]any)
	if !ok {
		return EpisodeTemplateRegistry{}, cfgErr("episode_templates", "%s", "expected a table of episode templates")
	}
	if len(entries) == 0 {
		return EpisodeTemplateRegistry{}, cfgErr("episode_templates",
			"present but empty; omit the table entirely rather than declaring no usable episode shape")
	}
	reg := NewEpisodeTemplateRegistry()
	// Sorted so a refusal names the same offending template on every run.
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t, err := parseEpisodeTemplate(name, entries[name])
		if err != nil {
			return EpisodeTemplateRegistry{}, err
		}
		if err := reg.Register(t); err != nil {
			return EpisodeTemplateRegistry{}, err
		}
	}
	return reg, nil
}

// parseEpisodeTemplate converts one entry of the episode_templates table.
func parseEpisodeTemplate(name string, raw any) (EpisodeTemplate, error) {
	field := "episode_templates." + strings.TrimSpace(name)
	tbl, ok := raw.(map[string]any)
	if !ok {
		return EpisodeTemplate{}, cfgErr(field, "%s", "expected a table of template settings")
	}
	t := EpisodeTemplate{
		Name:                name,
		ReportOnly:          boolField(tbl, "report_only"),
		Phases:              episodePhaseArray(tbl, "phases"),
		RequiredChecks:      stringArray(tbl, "required_checks"),
		MaxRepairIterations: intField(tbl, "max_repair_iterations"),
		RepairEnabled:       boolField(tbl, "repair_enabled"),
		MaxWallSeconds:      intField(tbl, "max_wall_seconds"),
		ReadOnly:            boolField(tbl, "read_only"),
		RepairBranchPrefix:  stringField(tbl, "repair_branch_prefix"),
	}
	// Roles are NAME=value pairs, so the map cannot be misread as role=profile
	// in one place and profile=role in another.
	roles, err := stringPairMap(tbl, "roles")
	if err != nil {
		return EpisodeTemplate{}, cfgErr(field+".roles", "%s", err.Error())
	}
	if roles != nil {
		t.Roles = map[ProfileRole]string{}
		for k, v := range roles {
			t.Roles[ProfileRole(strings.ToLower(strings.TrimSpace(k)))] = v
		}
	}
	// Return edges are FROM=TO pairs. They are read but not validated here;
	// Register does that, so a hand-built and a loaded template are held to
	// exactly one set of rules.
	edges, err := stringPairMap(tbl, "return_edges")
	if err != nil {
		return EpisodeTemplate{}, cfgErr(field+".return_edges", "%s", err.Error())
	}
	if edges != nil {
		t.ReturnEdges = map[contract.EpisodePhase][]contract.EpisodePhase{}
		for k, v := range edges {
			from := contract.EpisodePhase(strings.ToUpper(strings.TrimSpace(k)))
			t.ReturnEdges[from] = append(t.ReturnEdges[from], contract.EpisodePhase(strings.ToUpper(strings.TrimSpace(v))))
		}
	}
	return t, nil
}

// episodePhaseArray reads a phase list, upper-casing each entry so an operator
// who wrote "plan" gets the phase the contract defines rather than a typo.
func episodePhaseArray(doc map[string]any, key string) []contract.EpisodePhase {
	raw := stringArray(doc, key)
	if raw == nil {
		return nil
	}
	out := make([]contract.EpisodePhase, 0, len(raw))
	for _, p := range raw {
		out = append(out, contract.EpisodePhase(strings.ToUpper(strings.TrimSpace(p))))
	}
	return out
}

// stringPairMap decodes an array of NAME=value strings into a map.
//
// It mirrors how loadProfiles reads launch_env, including the duplicate-key
// refusal: a repeated key is a typo that would otherwise leave one of the two
// values silently discarded.
func stringPairMap(doc map[string]any, key string) (map[string]string, error) {
	raw, present := doc[key]
	if !present {
		return nil, nil
	}
	items, ok := raw.([]string)
	if !ok {
		return nil, fmt.Errorf("expected an array of NAME=value strings")
	}
	out := map[string]string{}
	for _, item := range items {
		k, v, ok := strings.Cut(item, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("invalid %s entry", "NAME=value")
		}
		if _, exists := out[k]; exists {
			return nil, fmt.Errorf("duplicate key %q", k)
		}
		out[k] = v
	}
	return out, nil
}
