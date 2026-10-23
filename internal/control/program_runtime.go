package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/emjin/agent-engine/contract"
)

// EpisodeProgramBinding is installed in the same state transaction as its
// episode. The complete definition is retained so a later PLAN_ACCEPT decision
// can be evaluated against the exact authority that admitted the child.
type EpisodeProgramBinding struct {
	Definition      contract.ProgramDefinition `json:"definition"`
	ProgramDigest   string                     `json:"program_digest"`
	CardID          string                     `json:"card_id"`
	CardDigest      string                     `json:"card_digest"`
	AuthorityRef    string                     `json:"authority_ref"`
	TemplateDigest  string                     `json:"template_digest"`
	DependencyHash  string                     `json:"dependency_baseline"`
	RepairRoutes    []contract.RepairRoute     `json:"repair_routes"`
	Resources       []contract.ResourceClaim   `json:"resources"`
	PolicyExpiresAt string                     `json:"policy_expires_at"`
}

// ProgramPlanAcceptanceReceipt is a durable, plan-digest-specific decision
// made by the Engine after an exact comparison with the locally pinned card.
type ProgramPlanAcceptanceReceipt struct {
	ProgramID     string `json:"program_id"`
	CardID        string `json:"card_id"`
	ProgramDigest string `json:"program_digest"`
	CardDigest    string `json:"card_digest"`
	PlanDigest    string `json:"plan_digest"`
	AuthorityRef  string `json:"authority_ref"`
	AcceptedAt    string `json:"accepted_at"`
}

// AdmitProgramEpisode installs one child episode from a locally pinned card.
// The remote caller supplies no policy receipt or acceptance flag.
func (s *Store) AdmitProgramEpisode(program *contract.ProgramDefinition, cardID string, ep *contract.EpisodeRequest) (*Admission, error) {
	program = cloneProgramDefinition(program)
	digest, err := s.VerifyProgramAuthority(program)
	if err != nil {
		return nil, err
	}
	card, ok := programCard(program, cardID)
	if !ok {
		return nil, fmt.Errorf("agent-engine: program card %q is not authorized", cardID)
	}
	grant := s.cfg.ProgramAuthorities[program.AuthorityRef]
	expires := grant.ExpiresAt
	cardDigest, err := card.CardDigest()
	if err != nil {
		return nil, err
	}
	binding := &EpisodeProgramBinding{
		Definition: *program, ProgramDigest: digest, CardID: cardID,
		CardDigest: cardDigest, AuthorityRef: program.AuthorityRef,
		PolicyExpiresAt: expires,
	}
	return s.admitEpisodeWithProgram(ep, false, binding)
}

func programCard(program *contract.ProgramDefinition, cardID string) (*contract.ProgramCard, bool) {
	if program == nil {
		return nil, false
	}
	for i := range program.Cards {
		if program.Cards[i].CardID == cardID {
			return &program.Cards[i], true
		}
	}
	return nil, false
}

func cloneProgramDefinition(program *contract.ProgramDefinition) *contract.ProgramDefinition {
	if program == nil {
		return nil
	}
	out := *program
	out.Cards = cloneProgramSlice(program.Cards)
	for i, card := range program.Cards {
		copyCard := card
		copyCard.DependsOn = cloneProgramSlice(card.DependsOn)
		copyCard.Roles = cloneProgramSlice(card.Roles)
		copyCard.Capabilities = cloneProgramSlice(card.Capabilities)
		copyCard.AllowedPaths = cloneProgramSlice(card.AllowedPaths)
		copyCard.ProtectedPaths = cloneProgramSlice(card.ProtectedPaths)
		copyCard.RequiredChecks = cloneProgramSlice(card.RequiredChecks)
		copyCard.AllowedChangeKinds = cloneProgramSlice(card.AllowedChangeKinds)
		copyCard.Obligations = cloneProgramSlice(card.Obligations)
		for j := range copyCard.Obligations {
			copyCard.Obligations[j].RequiredChecks = cloneProgramSlice(card.Obligations[j].RequiredChecks)
		}
		copyCard.RepairRoutes = cloneProgramSlice(card.RepairRoutes)
		copyCard.Resources = cloneProgramSlice(card.Resources)
		out.Cards[i] = copyCard
	}
	out.Escalations = cloneProgramSlice(program.Escalations)
	return &out
}

func cloneProgramSlice[T any](input []T) []T {
	if input == nil {
		return nil
	}
	output := make([]T, len(input))
	copy(output, input)
	return output
}

func programTemplateDigest(template EpisodeTemplate) (string, error) {
	b, err := json.Marshal(template)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}

type dependencyFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

var dependencyManifestNames = map[string]bool{
	"go.mod": true, "go.sum": true, "package.json": true, "package-lock.json": true,
	"pnpm-lock.yaml": true, "yarn.lock": true, "Cargo.toml": true, "Cargo.lock": true,
	"pyproject.toml": true, "poetry.lock": true, "composer.json": true, "composer.lock": true,
	"Gemfile": true, "Gemfile.lock": true, "Package.resolved": true,
}

func measureDependencyBaseline(root string) (string, error) {
	var files []dependencyFile
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != root && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !dependencyManifestNames[d.Name()] {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		h := sha256.Sum256(body)
		files = append(files, dependencyFile{Path: filepath.ToSlash(rel), SHA256: hex.EncodeToString(h[:])})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	b, err := json.Marshal(files)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func canonicalStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func programRolesFromProfiles(profiles map[ProfileRole]ProfileRef) []contract.ProgramRoleBinding {
	roles := make([]string, 0, len(profiles))
	for role := range profiles {
		roles = append(roles, string(role))
	}
	sort.Strings(roles)
	out := make([]contract.ProgramRoleBinding, 0, len(roles))
	for _, name := range roles {
		p := profiles[ProfileRole(name)]
		out = append(out, contract.ProgramRoleBinding{Role: name, Profile: p.Name, Model: p.Model})
	}
	return out
}

func programRoutesFromTemplate(template EpisodeTemplate) []contract.RepairRoute {
	var out []contract.RepairRoute
	for from, targets := range template.ReturnEdges {
		for _, target := range targets {
			route := contract.RepairRoute{From: string(from), To: string(target)}
			if from == target {
				// Episode templates use a same-phase return for a bounded repair
				// attempt. Program routes encode the attempt boundary explicitly,
				// because a self-edge would not name a distinct route.
				route.From += "#attempt"
				route.To += "#next-attempt"
			}
			out = append(out, route)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From == out[j].From {
			return out[i].To < out[j].To
		}
		return out[i].From < out[j].From
	})
	return out
}

func programCapabilities(allowWrite bool, checks []string) []string {
	out := []string{"workspace-read"}
	if allowWrite {
		out = append(out, "workspace-write")
	}
	if len(checks) > 0 {
		out = append(out, "managed-checks")
	}
	sort.Strings(out)
	return out
}

func validateProgramEpisodeAdmission(binding *EpisodeProgramBinding, card *contract.ProgramCard, ep *contract.EpisodeRequest, template EpisodeTemplate, profiles map[ProfileRole]ProfileRef, measured Snapshot, dependencyHash string) error {
	if ep == nil {
		return fmt.Errorf("agent-engine: no episode request supplied")
	}
	if ep.Objective != card.Objective || ep.Template != card.Template {
		return fmt.Errorf("agent-engine: episode objective or template differs from the authorised program card")
	}
	if measured.ContentDigest != card.SourceBaseline {
		return fmt.Errorf("agent-engine: workspace source baseline differs from the authorised program card")
	}
	if dependencyHash != card.DependencyBaseline {
		return fmt.Errorf("agent-engine: workspace dependency baseline differs from the authorised program card")
	}
	templateDigest, err := programTemplateDigest(template)
	if err != nil {
		return err
	}
	if templateDigest != card.TemplateDigest {
		return fmt.Errorf("agent-engine: locally approved template digest differs from the authorised program card")
	}
	if !sameProgramRoles(card.Roles, programRolesFromProfiles(profiles)) {
		return fmt.Errorf("agent-engine: locally bound role profiles or models differ from the authorised program card")
	}
	if !equalStrings(canonicalStrings(card.AllowedPaths), canonicalStrings(ep.Constraints.AllowedPaths)) ||
		!equalStrings(canonicalStrings(card.ProtectedPaths), canonicalStrings(ep.Constraints.ProtectedPaths)) {
		return fmt.Errorf("agent-engine: episode path constraints differ from the authorised program card")
	}
	if !equalStrings(canonicalStrings(card.RequiredChecks), canonicalStrings(template.RequiredChecks)) {
		return fmt.Errorf("agent-engine: locally required checks differ from the authorised program card")
	}
	if !equalWorkflowObligations(card.Obligations, ep.Obligations) {
		return fmt.Errorf("agent-engine: episode obligations differ from the authorised program card")
	}
	if !equalStrings(canonicalStrings(card.Capabilities), programCapabilities(ep.Constraints.AllowCodeChanges, template.RequiredChecks)) {
		return fmt.Errorf("agent-engine: local episode capabilities differ from the authorised program card")
	}
	routes := programRoutesFromTemplate(template)
	if !equalRepairRoutes(card.RepairRoutes, routes) {
		return fmt.Errorf("agent-engine: locally approved repair routes differ from the authorised program card")
	}
	if len(card.Resources) != 0 {
		return fmt.Errorf("agent-engine: resource claims require the coordinator resource-locking stage and are not admitted for a single episode")
	}
	if card.Budget.Children != 1 || card.Budget.RepairCycles > ep.Limits.Effective().RepairCycles || card.Budget.AuditAttempts > ep.Limits.Effective().AuditAttempts {
		return fmt.Errorf("agent-engine: program card budget exceeds the single episode's admitted limits")
	}
	if card.Budget.RepairCycles > template.MaxRepairIterations {
		return fmt.Errorf("agent-engine: program card repair budget exceeds the locally approved template")
	}
	binding.TemplateDigest = templateDigest
	binding.DependencyHash = dependencyHash
	binding.RepairRoutes = routes
	binding.Resources = []contract.ResourceClaim{}
	return nil
}

func sameProgramRoles(a, b []contract.ProgramRoleBinding) bool {
	if len(a) != len(b) {
		return false
	}
	copyA := append([]contract.ProgramRoleBinding(nil), a...)
	copyB := append([]contract.ProgramRoleBinding(nil), b...)
	sort.Slice(copyA, func(i, j int) bool { return copyA[i].Role < copyA[j].Role })
	sort.Slice(copyB, func(i, j int) bool { return copyB[i].Role < copyB[j].Role })
	for i := range copyA {
		if copyA[i] != copyB[i] {
			return false
		}
	}
	return true
}

func equalRepairRoutes(a, b []contract.RepairRoute) bool {
	if len(a) != len(b) {
		return false
	}
	copyA := append([]contract.RepairRoute(nil), a...)
	sort.Slice(copyA, func(i, j int) bool {
		if copyA[i].From == copyA[j].From {
			return copyA[i].To < copyA[j].To
		}
		return copyA[i].From < copyA[j].From
	})
	for i := range copyA {
		if copyA[i] != b[i] {
			return false
		}
	}
	return true
}

func equalWorkflowObligations(a, b []contract.WorkflowObligation) bool {
	if len(a) != len(b) {
		return false
	}
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func (e *Engine) measuredProgramPlan(st *State, plan *contract.ImplementationPlan) (*contract.ProgramPlanMatch, error) {
	if st == nil || st.ProgramBinding == nil || st.Episode == nil {
		return nil, fmt.Errorf("agent-engine: episode has no locally authorised program binding")
	}
	program := &st.ProgramBinding.Definition
	if _, err := e.store.VerifyProgramAuthority(program); err != nil {
		return nil, err
	}
	card, ok := programCard(program, st.ProgramBinding.CardID)
	if !ok {
		return nil, fmt.Errorf("agent-engine: retained program card is missing")
	}
	if digest, err := program.ProgramDigest(); err != nil || digest != st.ProgramBinding.ProgramDigest {
		return nil, fmt.Errorf("agent-engine: retained program definition digest is invalid")
	}
	if digest, err := card.CardDigest(); err != nil || digest != st.ProgramBinding.CardDigest {
		return nil, fmt.Errorf("agent-engine: retained program card digest is invalid")
	}
	template, ok := e.store.Config().EpisodeTemplates.Lookup(st.EpisodeTemplate)
	if !ok {
		return nil, fmt.Errorf("agent-engine: locally approved episode template is unavailable")
	}
	td, err := programTemplateDigest(template)
	if err != nil || td != st.ProgramBinding.TemplateDigest || td != card.TemplateDigest {
		return nil, fmt.Errorf("agent-engine: local episode template changed after program admission")
	}
	request, err := e.store.loadRequestDir(st.RequestID)
	if err != nil {
		return nil, err
	}
	depHash, err := measureDependencyBaseline(st.Workspace)
	if err != nil {
		return nil, err
	}
	if depHash != st.ProgramBinding.DependencyHash || depHash != card.DependencyBaseline {
		return nil, fmt.Errorf("agent-engine: dependency baseline changed after program admission")
	}
	baseline, err := measureStableEpisodeSource(st.Workspace, e.store.Config().StoreRoot, e.now())
	if err != nil {
		return nil, err
	}
	if baseline.ContentDigest != card.SourceBaseline || plan.SourceBaseline != baseline.ContentDigest {
		return nil, fmt.Errorf("agent-engine: source baseline changed since program admission")
	}
	roles := programRolesFromProfiles(st.ExecutionProfiles)
	obligations := make([]contract.WorkflowObligation, 0, len(request.Request.Obligations))
	for _, obligation := range request.Request.Obligations {
		obligations = append(obligations, contract.WorkflowObligation{ID: obligation.ID, Criterion: obligation.Criterion, RequiredChecks: append([]string(nil), obligation.RequiredChecks...)})
	}
	measured := contract.ProgramPlanMeasurement{
		EpisodeID: st.Episode.EpisodeID, Objective: request.Request.Objective,
		ReportDigest: st.Episode.ReportDigest, Template: st.EpisodeTemplate,
		TemplateDigest: td, DependencyBaseline: depHash, Roles: roles,
		Capabilities: programCapabilities(request.Request.Constraints.AllowCodeChanges, template.RequiredChecks),
		RepairRoutes: programRoutesFromTemplate(template), Resources: append([]contract.ResourceClaim{}, st.ProgramBinding.Resources...),
		Budget: contract.ProgramBudget{Children: 1, RepairCycles: st.Episode.Budget.RepairIterations,
			WorkerTurns: st.Episode.Budget.WorkerTurns, AuditAttempts: st.Episode.Budget.CompletionAudits},
		Obligations: obligations,
	}
	return contract.MatchProgramCard(program, st.ProgramBinding.CardID, plan, measured)
}

func (e *Engine) acceptProgramPlanIfExact(requestID string) (bool, error) {
	st, err := e.store.State(requestID)
	if err != nil {
		return false, err
	}
	if st.ProgramBinding == nil || st.Episode == nil || st.Episode.Phase != string(PhasePlanAccept) || st.Episode.PlanAuthorized {
		return false, nil
	}
	if !programAuthorityLive(st.ProgramBinding, e.now()) {
		return false, nil
	}
	body, err := e.ReadEpisodeArtifact(requestID, st.Episode.PlanDigest)
	if err != nil {
		return false, err
	}
	var plan contract.ImplementationPlan
	if err := json.Unmarshal(body, &plan); err != nil {
		return false, err
	}
	match, err := e.measuredProgramPlan(st, &plan)
	if err != nil {
		return false, nil
	} // mismatch remains at human PLAN_ACCEPT
	auth := contract.PlanAuthorization{
		Schema: contract.EpisodeVersion, EpisodeID: plan.EpisodeID, PlanVersion: plan.Version,
		PlanDigest: match.PlanDigest, ReportDigest: plan.AcceptedReportDigest,
		SourceBaseline: plan.SourceBaseline, Grant: plan.Grant,
		AcceptedBy: "policy:program:" + st.ProgramBinding.AuthorityRef,
		AcceptedAt: e.stamp(),
	}
	receipt := ProgramPlanAcceptanceReceipt{ProgramID: match.ProgramID, CardID: match.CardID,
		ProgramDigest: match.ProgramDigest, CardDigest: match.CardDigest, PlanDigest: match.PlanDigest,
		AuthorityRef: st.ProgramBinding.AuthorityRef, AcceptedAt: auth.AcceptedAt}
	_, err = e.acceptEpisodePlan(requestID, auth, &plan, &receipt)
	if err != nil {
		return false, err
	}
	return true, nil
}

func programAuthorityLive(binding *EpisodeProgramBinding, now time.Time) bool {
	if binding == nil {
		return false
	}
	expires, err := time.Parse(time.RFC3339, binding.PolicyExpiresAt)
	return err == nil && now.Before(expires)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
