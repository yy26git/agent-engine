package contract

// Delegated-program contracts describe locally bounded multi-card work. They
// carry intent and constraints only: decoding or matching one never grants
// execution authority. The Engine must resolve AuthorityRef against local,
// operator-controlled state before it can use a program.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// ProgramVersion is the separately versioned delegated-program contract.
const ProgramVersion = 1

// ErrProgramInvalid marks a malformed or non-matching delegated program.
var ErrProgramInvalid = fmt.Errorf("contract: delegated program invalid")

// ProgramDefinition is an immutable, locally provisioned work order. The
// authority reference is only a lookup key; the string itself is not proof that
// an operator granted anything. Store the digest at provisioning and refuse
// changes to the definition after it has been admitted.
type ProgramDefinition struct {
	Schema       int           `json:"schema"`
	ProgramID    string        `json:"program_id"`
	Revision     int           `json:"revision"`
	AuthorityRef string        `json:"authority_ref"`
	IssuedAt     string        `json:"issued_at"`
	Budget       ProgramBudget `json:"budget"`
	Cards        []ProgramCard `json:"cards"`
	Escalations  []Escalation  `json:"escalations"`
}

// ProgramCard is one exact child-work authorization within a program.
// Security-relevant fields are explicit so the Engine can compare them with
// measured episode state and the proposed plan rather than interpreting prose.
type ProgramCard struct {
	CardID             string               `json:"card_id"`
	DependsOn          []string             `json:"depends_on"`
	Objective          string               `json:"objective"`
	Template           string               `json:"template"`
	TemplateDigest     string               `json:"template_digest"`
	SourceBaseline     string               `json:"source_baseline"`
	DependencyBaseline string               `json:"dependency_baseline"`
	Grant              string               `json:"grant"`
	Roles              []ProgramRoleBinding `json:"roles"`
	Capabilities       []string             `json:"capabilities"`
	AllowedPaths       []string             `json:"allowed_paths"`
	ProtectedPaths     []string             `json:"protected_paths"`
	RequiredChecks     []string             `json:"required_checks"`
	AllowedChangeKinds []string             `json:"allowed_change_kinds"`
	Obligations        []WorkflowObligation `json:"obligations"`
	RepairRoutes       []RepairRoute        `json:"repair_routes"`
	Resources          []ResourceClaim      `json:"resources"`
	Budget             ProgramBudget        `json:"budget"`
}

// ProgramRoleBinding pins a role to a local execution profile and, when
// configured, the model expected from that profile. Empty model is permitted
// only when the profile itself has no model pin.
type ProgramRoleBinding struct {
	Role    string `json:"role"`
	Profile string `json:"profile"`
	Model   string `json:"model,omitempty"`
}

// ProgramBudget bounds the whole program or an individual child card. Values
// are concrete ceilings; zero is not an implicit unlimited value.
type ProgramBudget struct {
	Children      int `json:"children"`
	RepairCycles  int `json:"repair_cycles"`
	WorkerTurns   int `json:"worker_turns"`
	AuditAttempts int `json:"audit_attempts"`
}

// RepairRoute identifies an allowed transition when a card needs another
// bounded attempt. It is data, not a command or executable instruction.
type RepairRoute struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ResourceClaim names a shared resource a child needs. Exclusive claims are
// used by the coordinator to prevent unsafe overlap; this type does not itself
// schedule or lock resources.
type ResourceClaim struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Exclusive bool   `json:"exclusive"`
}

// Escalation is an explicit stop-and-request-human-decision condition.
type Escalation struct {
	ID          string `json:"id"`
	When        string `json:"when"`
	Description string `json:"description"`
}

// ProgramPlanMeasurement contains values measured by the Engine from the
// admitted child, immutable local profile/template state and proposed plan.
// Callers must not populate it from a Forge request or model-authored field.
// It is deliberately not an authorization receipt.
type ProgramPlanMeasurement struct {
	EpisodeID          string               `json:"episode_id"`
	Objective          string               `json:"objective"`
	ReportDigest       string               `json:"report_digest"`
	Template           string               `json:"template"`
	TemplateDigest     string               `json:"template_digest"`
	DependencyBaseline string               `json:"dependency_baseline"`
	Roles              []ProgramRoleBinding `json:"roles"`
	Capabilities       []string             `json:"capabilities"`
	RepairRoutes       []RepairRoute        `json:"repair_routes"`
	Resources          []ResourceClaim      `json:"resources"`
	Budget             ProgramBudget        `json:"budget"`
	Obligations        []WorkflowObligation `json:"obligations"`
}

// ProgramPlanMatch is evidence that a measured plan exactly matches a card.
// It contains no AcceptedBy field and is not sufficient to launch a worker.
// The caller must atomically record a separate local policy decision receipt.
type ProgramPlanMatch struct {
	ProgramID     string `json:"program_id"`
	CardID        string `json:"card_id"`
	ProgramDigest string `json:"program_digest"`
	CardDigest    string `json:"card_digest"`
	PlanDigest    string `json:"plan_digest"`
}

// ValidateProgramDefinition checks the version, identity, budgets, cards and
// escalation rules. It refuses missing bounds and duplicate identities rather
// than interpreting omission as unlimited authority.
func ValidateProgramDefinition(p *ProgramDefinition) error {
	if p == nil {
		return fmt.Errorf("%w: no program supplied", ErrProgramInvalid)
	}
	if p.Schema != ProgramVersion {
		return fmt.Errorf("%w: schema %d is not supported; this build understands %d", ErrProgramInvalid, p.Schema, ProgramVersion)
	}
	if strings.TrimSpace(p.ProgramID) == "" || p.Revision < 1 || strings.TrimSpace(p.AuthorityRef) == "" || strings.TrimSpace(p.IssuedAt) == "" {
		return fmt.Errorf("%w: program identity, positive revision, local authority reference and issue time are required", ErrProgramInvalid)
	}
	if err := validateProgramBudget(p.Budget); err != nil {
		return fmt.Errorf("%w: program budget: %v", ErrProgramInvalid, err)
	}
	if len(p.Cards) == 0 {
		return fmt.Errorf("%w: program contains no cards", ErrProgramInvalid)
	}
	if len(p.Escalations) == 0 {
		return fmt.Errorf("%w: program must define at least one escalation condition", ErrProgramInvalid)
	}
	if p.Budget.Children < len(p.Cards) {
		return fmt.Errorf("%w: program child ceiling is lower than its number of authorized cards", ErrProgramInvalid)
	}
	seenCards := map[string]bool{}
	for i := range p.Cards {
		c := &p.Cards[i]
		if strings.TrimSpace(c.CardID) == "" || seenCards[c.CardID] {
			return fmt.Errorf("%w: card %d has an empty or duplicate id %q", ErrProgramInvalid, i, c.CardID)
		}
		seenCards[c.CardID] = true
		if err := ValidateProgramCard(c); err != nil {
			return fmt.Errorf("%w: card %q: %v", ErrProgramInvalid, c.CardID, err)
		}
		if !budgetWithin(c.Budget, p.Budget) {
			return fmt.Errorf("%w: card %q budget exceeds the program ceiling", ErrProgramInvalid, c.CardID)
		}
	}
	if err := validateCardDependencies(p.Cards); err != nil {
		return fmt.Errorf("%w: %v", ErrProgramInvalid, err)
	}
	seenEscalations := map[string]bool{}
	for i, e := range p.Escalations {
		if strings.TrimSpace(e.ID) == "" || strings.TrimSpace(e.When) == "" || strings.TrimSpace(e.Description) == "" || seenEscalations[e.ID] {
			return fmt.Errorf("%w: escalation %d is incomplete or duplicated", ErrProgramInvalid, i)
		}
		seenEscalations[e.ID] = true
	}
	return nil
}

// ValidateProgramCard enforces the fields required for exact local matching.
func ValidateProgramCard(c *ProgramCard) error {
	if c == nil {
		return fmt.Errorf("no card supplied")
	}
	for name, value := range map[string]string{
		"objective": c.Objective, "template": c.Template, "template digest": c.TemplateDigest,
		"source baseline": c.SourceBaseline, "dependency baseline": c.DependencyBaseline, "grant": c.Grant,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if !isLowerSHA256(c.TemplateDigest) || !isLowerSHA256(c.SourceBaseline) || !isLowerSHA256(c.DependencyBaseline) {
		return fmt.Errorf("template, source and dependency baselines must be lowercase SHA-256 digests")
	}
	if err := validateProgramBudget(c.Budget); err != nil {
		return fmt.Errorf("budget: %v", err)
	}
	if len(c.Roles) == 0 || len(c.Capabilities) == 0 || len(c.AllowedPaths) == 0 || len(c.RequiredChecks) == 0 || len(c.AllowedChangeKinds) == 0 || len(c.Obligations) == 0 {
		return fmt.Errorf("roles, capabilities, allowed paths, checks, change kinds and obligations must be explicit")
	}
	if c.DependsOn == nil || c.ProtectedPaths == nil || c.RepairRoutes == nil || c.Resources == nil {
		return fmt.Errorf("depends_on, protected_paths, repair_routes and resources must be explicit arrays (use [] when empty)")
	}
	if err := uniqueNonEmpty("card dependency", c.DependsOn); err != nil {
		return err
	}
	if err := uniqueNonEmpty("role", roleNames(c.Roles)); err != nil {
		return err
	}
	for i, role := range c.Roles {
		if strings.TrimSpace(role.Profile) == "" || strings.TrimSpace(role.Role) == "" {
			return fmt.Errorf("role binding %d must name a role and profile", i)
		}
	}
	if profileForRole(c.Roles, "worker") == "" || profileForRole(c.Roles, "reviewer") == "" {
		return fmt.Errorf("worker and reviewer role bindings are required")
	}
	for name, values := range map[string][]string{
		"capability": c.Capabilities, "allowed path": c.AllowedPaths, "protected path": c.ProtectedPaths,
		"required check": c.RequiredChecks, "allowed change kind": c.AllowedChangeKinds,
	} {
		if err := uniqueNonEmpty(name, values); err != nil {
			return err
		}
	}
	if err := uniqueNonEmpty("obligation", obligationIDs(c.Obligations)); err != nil {
		return err
	}
	for i, o := range c.Obligations {
		if strings.TrimSpace(o.Criterion) == "" {
			return fmt.Errorf("obligation %d has no criterion", i)
		}
	}
	if err := validateRoutes(c.RepairRoutes); err != nil {
		return err
	}
	if err := validateResources(c.Resources); err != nil {
		return err
	}
	return nil
}

// ProgramDigest returns the stable digest of a validated immutable program.
func (p *ProgramDefinition) ProgramDigest() (string, error) {
	if err := ValidateProgramDefinition(p); err != nil {
		return "", err
	}
	return digestJSON(p)
}

// CardDigest returns the stable digest of a validated card.
func (c *ProgramCard) CardDigest() (string, error) {
	if err := ValidateProgramCard(c); err != nil {
		return "", fmt.Errorf("%w: %v", ErrProgramInvalid, err)
	}
	return digestJSON(c)
}

// DecodeProgramDefinition strictly decodes one definition and refuses unknown
// fields or trailing JSON. A future schema must be handled explicitly.
func DecodeProgramDefinition(data []byte) (*ProgramDefinition, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p ProgramDefinition
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("%w: decoding: %v", ErrProgramInvalid, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("%w: trailing JSON value", ErrProgramInvalid)
		}
		return nil, fmt.Errorf("%w: trailing content: %v", ErrProgramInvalid, err)
	}
	if err := ValidateProgramDefinition(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// MatchProgramCard compares every security-relevant plan and measured child
// field with the immutable authorized card. It only returns match evidence;
// it does not accept the plan or confer execution authority.
func MatchProgramCard(program *ProgramDefinition, cardID string, plan *ImplementationPlan, measured ProgramPlanMeasurement) (*ProgramPlanMatch, error) {
	if err := ValidateProgramDefinition(program); err != nil {
		return nil, err
	}
	if err := ValidateImplementationPlan(plan); err != nil {
		return nil, fmt.Errorf("%w: proposed plan: %v", ErrProgramInvalid, err)
	}
	var card *ProgramCard
	for i := range program.Cards {
		if program.Cards[i].CardID == cardID {
			card = &program.Cards[i]
			break
		}
	}
	if card == nil {
		return nil, fmt.Errorf("%w: card %q is not in program %q", ErrProgramInvalid, cardID, program.ProgramID)
	}
	if plan.SourceBaseline != card.SourceBaseline || plan.Grant != card.Grant {
		return nil, fmt.Errorf("%w: plan source baseline or grant differs from card %q", ErrProgramInvalid, cardID)
	}
	if plan.EpisodeID != measured.EpisodeID || strings.TrimSpace(measured.EpisodeID) == "" {
		return nil, fmt.Errorf("%w: measured episode identity does not match plan", ErrProgramInvalid)
	}
	if measured.Objective != card.Objective {
		return nil, fmt.Errorf("%w: admitted child objective differs from card %q", ErrProgramInvalid, cardID)
	}
	if !isLowerSHA256(measured.ReportDigest) || plan.AcceptedReportDigest != measured.ReportDigest {
		return nil, fmt.Errorf("%w: plan report digest does not match the measured accepted report", ErrProgramInvalid)
	}
	if measured.Objective != card.Objective || measured.Template != card.Template || measured.TemplateDigest != card.TemplateDigest || measured.DependencyBaseline != card.DependencyBaseline {
		return nil, fmt.Errorf("%w: measured objective, template or dependency baseline differs from card %q", ErrProgramInvalid, cardID)
	}
	if measured.Roles == nil || measured.Capabilities == nil || measured.RepairRoutes == nil || measured.Resources == nil || measured.Obligations == nil {
		return nil, fmt.Errorf("%w: measured security fields must be explicit; omitted arrays are not treated as empty", ErrProgramInvalid)
	}
	if plan.ExecutionProfile != profileForRole(card.Roles, "worker") || plan.ReviewProfile != profileForRole(card.Roles, "reviewer") {
		return nil, fmt.Errorf("%w: plan execution/review profiles differ from authorized worker/reviewer roles", ErrProgramInvalid)
	}
	if !reflect.DeepEqual(card.Roles, measured.Roles) || !reflect.DeepEqual(card.Capabilities, measured.Capabilities) ||
		!reflect.DeepEqual(card.RepairRoutes, measured.RepairRoutes) || !reflect.DeepEqual(card.Resources, measured.Resources) ||
		card.Budget != measured.Budget || !reflect.DeepEqual(card.Obligations, measured.Obligations) {
		return nil, fmt.Errorf("%w: measured roles, capabilities, repair routes, resources, budget or obligations differ from card %q", ErrProgramInvalid, cardID)
	}
	paths, protected, checks, kinds := planProjection(plan)
	requirementIDs := planRequirementIDs(plan)
	if !reflect.DeepEqual(card.AllowedPaths, paths) || !reflect.DeepEqual(card.ProtectedPaths, protected) ||
		!reflect.DeepEqual(card.RequiredChecks, checks) || !reflect.DeepEqual(card.AllowedChangeKinds, kinds) {
		return nil, fmt.Errorf("%w: proposed plan path, check or change-kind scope differs from card %q", ErrProgramInvalid, cardID)
	}
	if !sameStringSet(obligationIDs(card.Obligations), requirementIDs) {
		return nil, fmt.Errorf("%w: plan requirement IDs must cover exactly the card obligations", ErrProgramInvalid)
	}
	programDigest, err := program.ProgramDigest()
	if err != nil {
		return nil, err
	}
	cardDigest, err := card.CardDigest()
	if err != nil {
		return nil, err
	}
	planDigest, err := plan.Digest()
	if err != nil {
		return nil, fmt.Errorf("%w: digesting proposed plan: %v", ErrProgramInvalid, err)
	}
	return &ProgramPlanMatch{ProgramID: program.ProgramID, CardID: cardID, ProgramDigest: programDigest, CardDigest: cardDigest, PlanDigest: planDigest}, nil
}

func validateProgramBudget(b ProgramBudget) error {
	if b.Children < 1 || b.RepairCycles < 1 || b.WorkerTurns < 1 || b.AuditAttempts < 1 {
		return fmt.Errorf("children, repair cycles, worker turns and audit attempts must all be positive")
	}
	return nil
}

func budgetWithin(child, program ProgramBudget) bool {
	return child.Children <= program.Children && child.RepairCycles <= program.RepairCycles &&
		child.WorkerTurns <= program.WorkerTurns && child.AuditAttempts <= program.AuditAttempts
}

func validateCardDependencies(cards []ProgramCard) error {
	byID := make(map[string]ProgramCard, len(cards))
	for _, card := range cards {
		byID[card.CardID] = card
	}
	for _, card := range cards {
		for _, dependency := range card.DependsOn {
			if dependency == card.CardID {
				return fmt.Errorf("card %q depends on itself", card.CardID)
			}
			if _, ok := byID[dependency]; !ok {
				return fmt.Errorf("card %q depends on unknown card %q", card.CardID, dependency)
			}
		}
	}
	state := map[string]uint8{}
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("card dependency cycle includes %q", id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, dependency := range byID[id].DependsOn {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for _, card := range cards {
		if err := visit(card.CardID); err != nil {
			return err
		}
	}
	return nil
}

func validateRoutes(routes []RepairRoute) error {
	seen := map[string]bool{}
	for i, r := range routes {
		key := r.From + "\x00" + r.To
		if strings.TrimSpace(r.From) == "" || strings.TrimSpace(r.To) == "" || r.From == r.To || seen[key] {
			return fmt.Errorf("repair route %d is incomplete, self-referential or duplicated", i)
		}
		seen[key] = true
	}
	return nil
}

func validateResources(resources []ResourceClaim) error {
	seen := map[string]bool{}
	for i, r := range resources {
		key := r.Kind + "\x00" + r.ID
		if strings.TrimSpace(r.Kind) == "" || strings.TrimSpace(r.ID) == "" || seen[key] {
			return fmt.Errorf("resource claim %d is incomplete or duplicated", i)
		}
		seen[key] = true
	}
	return nil
}

func uniqueNonEmpty(label string, values []string) error {
	seen := map[string]bool{}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || seen[value] {
			return fmt.Errorf("%s is empty or duplicated", label)
		}
		seen[value] = true
	}
	return nil
}

func isLowerSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func roleNames(roles []ProgramRoleBinding) []string {
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = r.Role
	}
	return out
}
func obligationIDs(items []WorkflowObligation) []string {
	out := make([]string, len(items))
	for i, o := range items {
		out[i] = o.ID
	}
	return out
}

func profileForRole(roles []ProgramRoleBinding, role string) string {
	for _, binding := range roles {
		if binding.Role == role {
			return binding.Profile
		}
	}
	return ""
}

// planProjection compares the effective plan-wide scopes without depending on
// JSON map order. The card arrays are required to be unique; projection uses
// first occurrence order so any card serialization change remains digestable.
func planProjection(plan *ImplementationPlan) (allowed, protected, checks, kinds []string) {
	allowedSet, protectedSet, checksSet, kindsSet := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	appendUnique := func(dst *[]string, seen map[string]bool, values []string) {
		for _, value := range values {
			if !seen[value] {
				seen[value] = true
				*dst = append(*dst, value)
			}
		}
	}
	for _, step := range plan.Steps {
		appendUnique(&allowed, allowedSet, step.AllowedPaths)
		appendUnique(&protected, protectedSet, step.ProtectedPaths)
		appendUnique(&checks, checksSet, step.RequiredChecks)
		appendUnique(&kinds, kindsSet, step.AllowedChangeKinds)
	}
	return
}

func planRequirementIDs(plan *ImplementationPlan) []string {
	seen := map[string]bool{}
	var ids []string
	for _, step := range plan.Steps {
		for _, id := range step.RequirementIDs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, value := range a {
		if seen[value] {
			return false
		}
		seen[value] = true
	}
	for _, value := range b {
		if !seen[value] {
			return false
		}
	}
	return true
}
