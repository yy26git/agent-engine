package contract

// The public workflow contract.
//
// A workflow is a graph of engineering tasks. This file defines the SHAPE of that
// graph and how it is identified; it deliberately contains no scheduling, no
// execution and no readiness logic, because a second scheduler is the specific
// failure this design exists to prevent.
//
// Three decisions shape everything here:
//
//  1. A definition and its run INPUT are separate. The definition says what the
//     graph is; the input says what this particular run is about. Keeping them
//     apart is what lets two runs share one definition identity while remaining
//     two distinct requests, instead of redefining the whole graph per run.
//
//  2. Identity is the pinned BYTES, not the caller's claim about them. A
//     definition digest covers the canonical JSON plus a manifest of the actual
//     instruction file digests. A caller cannot submit a digest and have it
//     believed: the bytes are read and measured here.
//
//  3. There is no expression language, no branch predicate, no on_fail, no loop
//     and no caller-supplied argv. Every one of those turns a declarative graph
//     into a program, and a program in a trust boundary is a program that can
//     decide to skip the boundary.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// WorkflowVersion is the definition/ref schema this build understands.
const WorkflowVersion = 1

// RequestVersion2 is the v2 request envelope for workflow invocation.
//
// v1 remains valid and unchanged. A v2 request is a DIFFERENT thing — it names a
// workflow rather than describing one task — so it gets its own version rather
// than new optional fields on v1, which would let a v1 decoder silently accept a
// document whose safety fields it ignores.
const RequestVersion2 = 2

// MaxWorkflowTasks bounds a single definition.
//
// The bound exists because a graph is expanded at admission into one durable
// child request per task. An unbounded list is an unbounded write at a moment when
// the caller holds the admission lock.
const MaxWorkflowTasks = 64

// MaxDefinitionBytes and MaxInstructionBytes bound the definition and each of its
// instruction files.
const (
	MaxDefinitionBytes  = 1 << 20 // 1 MiB
	MaxInstructionBytes = 1 << 20 // 1 MiB
	MaxInputBytes       = 1 << 20 // 1 MiB
)

// taskIDPattern is the only accepted task identifier.
//
// Lowercase ASCII with dashes, no leading digit: a task ID becomes part of a child
// request ID and a title that OMA normalises, and a permissive pattern here is a
// collision waiting to be normalised into one.
var taskIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// TaskID reports whether an identifier is a well-formed task ID.
func TaskID(v string) bool { return taskIDPattern.MatchString(v) }

// WorkflowTask is one task in a definition.
type WorkflowTask struct {
	// ID is the stable identifier used by depends_on and by child derivation.
	ID string `json:"id"`
	// Operation reuses the closed v1 operation set.
	Operation Operation `json:"operation"`
	// Objective is the one-line engineering objective.
	Objective string `json:"objective"`
	// InstructionFile is the relative path, inside the definition bundle, of the
	// full instruction body. It is a PATH WITHIN THE BUNDLE, never a host path:
	// a caller-chosen host path is a caller-chosen file to read.
	InstructionFile string `json:"instruction_file,omitempty"`
	// Instruction is an inline body, used when no file is named. Exactly one of
	// Instruction and InstructionFile may be set.
	Instruction string `json:"instruction,omitempty"`
	// Obligations are the obligation templates this task must satisfy.
	Obligations []WorkflowObligation `json:"obligations,omitempty"`
	// ExecutorProfile names the operator-approved worker profile.
	ExecutorProfile string `json:"executor_profile"`
	// ReviewerProfile names the operator-approved reviewer profile. It may not be
	// the executor's profile.
	ReviewerProfile string `json:"reviewer_profile"`
	// Omitted review roles retain the legacy completion reviewer selection.
	CheckpointProfile string `json:"checkpoint_profile,omitempty"`
	DiagnosticProfile string `json:"diagnostic_profile,omitempty"`
	// CheckProfiles names approved check profiles this task's evidence needs.
	CheckProfiles []string `json:"check_profiles,omitempty"`
	// DependsOn names task IDs that must complete first.
	DependsOn []string `json:"depends_on,omitempty"`
	// InputKeys declares which runtime input keys this task's instruction
	// references. They are DECLARED, so a missing key is caught at admission
	// rather than being discovered as an empty substitution mid-run.
	InputKeys []string `json:"input_keys,omitempty"`
}

// WorkflowObligation is an obligation template.
//
// It is a template rather than a live obligation: the admitted child request
// expands it, and the disposition is set by evidence rather than by the caller.
type WorkflowObligation struct {
	ID             string   `json:"id"`
	Criterion      string   `json:"criterion"`
	RequiredChecks []string `json:"required_checks,omitempty"`
}

// WorkflowDefinition is a complete, reusable graph.
type WorkflowDefinition struct {
	Version int            `json:"version"`
	Name    string         `json:"name"`
	Tasks   []WorkflowTask `json:"tasks"`
}

// WorkflowRef references a previously admitted definition by digest.
type WorkflowRef struct {
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
}

// InstructionFile is one file in a definition bundle.
type InstructionFile struct {
	// Path is the bundle-relative path, slash separated.
	Path string `json:"path"`
	// SHA256 is the MEASURED digest of the bytes at that path.
	SHA256 string `json:"sha256"`
	// Bytes is the file size, recorded so a truncated read is visible.
	Bytes int `json:"bytes"`
}

// DefinitionManifest is the sorted file manifest a definition digest covers.
//
// It is separate from the definition JSON so the digest can be computed over
// "these exact bytes" rather than over "these file names", which is what makes a
// caller-changed instruction file change the identity.
type DefinitionManifest struct {
	Schema  int               `json:"schema"`
	Files   []InstructionFile `json:"files"`
	DefJSON string            `json:"definition_sha256"`
}

// ValidateWorkflowDefinition enforces the definition's structural rules.
//
// Everything here is a refusal rather than a repair. A malformed graph is
// corrected by being rejected, because a silently corrected graph is a graph
// nobody reviewed.
func ValidateWorkflowDefinition(d *WorkflowDefinition) error {
	if d == nil {
		return fmt.Errorf("contract: no workflow definition supplied")
	}
	if d.Version != WorkflowVersion {
		return fmt.Errorf("contract: workflow definition version %d is not supported; this build understands %d",
			d.Version, WorkflowVersion)
	}
	if len(d.Tasks) == 0 {
		return fmt.Errorf("contract: a workflow definition must contain at least one task")
	}
	if len(d.Tasks) > MaxWorkflowTasks {
		return fmt.Errorf("contract: a workflow may contain at most %d tasks, got %d", MaxWorkflowTasks, len(d.Tasks))
	}
	byID := make(map[string]WorkflowTask, len(d.Tasks))
	for i, t := range d.Tasks {
		if !TaskID(t.ID) {
			return fmt.Errorf("contract: task %d has id %q; ids must match %s", i, t.ID, taskIDPattern)
		}
		if _, dup := byID[t.ID]; dup {
			return fmt.Errorf("contract: task id %q appears more than once", t.ID)
		}
		if !validOperations[t.Operation] {
			return fmt.Errorf("contract: task %q has operation %q, which is not one of the supported operations",
				t.ID, t.Operation)
		}
		if strings.TrimSpace(t.Objective) == "" {
			return fmt.Errorf("contract: task %q has no objective", t.ID)
		}
		// Exactly one instruction source. Both would make the effective
		// instruction ambiguous, and neither is a useful request.
		hasFile := strings.TrimSpace(t.InstructionFile) != ""
		hasInline := strings.TrimSpace(t.Instruction) != ""
		switch {
		case hasFile && hasInline:
			return fmt.Errorf("contract: task %q names both an instruction file and an inline instruction", t.ID)
		case !hasFile && !hasInline:
			return fmt.Errorf("contract: task %q has no instruction; an objective alone is not a task", t.ID)
		}
		if strings.TrimSpace(t.ExecutorProfile) == "" || strings.TrimSpace(t.ReviewerProfile) == "" {
			return fmt.Errorf("contract: task %q must name both an executor and a reviewer profile", t.ID)
		}
		if t.ExecutorProfile == t.ReviewerProfile {
			// One profile cannot both implement and review a task, whatever the
			// registry says, because the two roles would be one session.
			return fmt.Errorf("contract: task %q names the same profile %q for executor and reviewer",
				t.ID, t.ExecutorProfile)
		}
		for _, dep := range t.DependsOn {
			if dep == t.ID {
				return fmt.Errorf("contract: task %q depends on itself", t.ID)
			}
		}
		byID[t.ID] = t
	}

	// Dependencies must exist. A missing dependency is refused rather than
	// dropped, because dropping it silently changes the graph's meaning from
	// "after X" to "immediately".
	for _, t := range d.Tasks {
		for _, dep := range t.DependsOn {
			if _, ok := byID[dep]; !ok {
				return fmt.Errorf("contract: task %q depends on %q, which the definition does not contain", t.ID, dep)
			}
		}
	}
	if err := rejectCycles(d.Tasks); err != nil {
		return err
	}
	// A cycle is a graph OMA cannot schedule, and a graph this layer refuses to
	// schedule must never reach an outer scheduler that would have to guess.
	return nil
}

// rejectCycles reports a dependency cycle, naming the tasks involved.
func rejectCycles(tasks []WorkflowTask) error {
	byID := make(map[string]WorkflowTask, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	// 0 unvisited, 1 on the current path, 2 fully explored.
	const (
		white = 0
		grey  = 1
		black = 2
	)
	colour := make(map[string]int, len(tasks))
	var stack []string

	var visit func(string) error
	visit = func(id string) error {
		switch colour[id] {
		case grey:
			// Trim the stack to the cycle itself so the message names the loop.
			at := 0
			for i, s := range stack {
				if s == id {
					at = i
					break
				}
			}
			return fmt.Errorf("contract: the dependency graph has a cycle: %s",
				strings.Join(append(append([]string{}, stack[at:]...), id), " -> "))
		case black:
			return nil
		}
		colour[id] = grey
		stack = append(stack, id)
		for _, dep := range byID[id].DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		colour[id] = black
		return nil
	}
	// Sorted entry points so the reported cycle is deterministic rather than
	// depending on map iteration order.
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

// DefinitionIdentity is the digest of a definition and its instruction bytes.
//
// It is domain-separated by schema version so a future schema cannot collide with
// this one, and it covers the file manifest rather than the file names.
func DefinitionIdentity(d *WorkflowDefinition, files []InstructionFile) (string, error) {
	if d == nil {
		return "", fmt.Errorf("contract: cannot hash a nil definition")
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("contract: encoding workflow definition: %w", err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return "", fmt.Errorf("contract: normalising workflow definition: %w", err)
	}
	canonical, err := json.Marshal(generic)
	if err != nil {
		return "", fmt.Errorf("contract: encoding canonical workflow definition: %w", err)
	}
	defSum := sha256.Sum256(canonical)

	// The manifest is sorted by path so directory order cannot change identity.
	sorted := append([]InstructionFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	manifest := DefinitionManifest{
		Schema:  WorkflowVersion,
		Files:   sorted,
		DefJSON: hex.EncodeToString(defSum[:]),
	}
	manRaw, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("contract: encoding definition manifest: %w", err)
	}
	var manGeneric any
	if err := json.Unmarshal(manRaw, &manGeneric); err != nil {
		return "", fmt.Errorf("contract: normalising definition manifest: %w", err)
	}
	manCanonical, err := json.Marshal(manGeneric)
	if err != nil {
		return "", fmt.Errorf("contract: encoding canonical manifest: %w", err)
	}
	// Domain separation: a definition identity is not a request digest, and the
	// two must never be interchangeable.
	h := sha256.New()
	h.Write([]byte("agent-engine/workflow-definition/v1\x00"))
	h.Write(manCanonical)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ValidateInstructionPath refuses a bundle path that is not a safe relative path.
//
// The rules are narrow on purpose: no absolute path, no parent traversal, no
// leading slash, no drive-ish prefix, no empty or dot segments. The bundle is
// written under an operator-owned root, and a path that escapes it is a path that
// writes outside it.
func ValidateInstructionPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("contract: an instruction path is empty")
	}
	if len(p) > 256 {
		return fmt.Errorf("contract: instruction path is longer than 256 bytes")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return fmt.Errorf("contract: instruction path %q must be relative and slash separated", p)
	}
	if strings.Contains(p, "//") {
		return fmt.Errorf("contract: instruction path %q contains an empty segment", p)
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".", "..":
			return fmt.Errorf("contract: instruction path %q contains an empty or traversing segment", p)
		}
	}
	// A NUL or a newline in a path that becomes a filename is a way to make two
	// distinct paths name the same file.
	if strings.ContainsAny(p, "\x00\n\r") {
		return fmt.Errorf("contract: instruction path %q contains a control character", p)
	}
	return nil
}

// ChildRequestID derives a child's request ID from its root, definition and task.
//
// The form is length-bounded and collision-checked by the caller: the hash keeps
// the ID short and stable regardless of how long the inputs are, and the task ID
// is carried in plain text so a human reading a store can tell which child is
// which without a lookup table.
func ChildRequestID(rootID, definitionSHA, taskID string) string {
	sum := sha256.Sum256([]byte("agent-engine/child\x00" + rootID + "\x00" + definitionSHA + "\x00" + taskID))
	return fmt.Sprintf("%s--%s--%s", rootID, taskID, hex.EncodeToString(sum[:])[:16])
}

// ValidateWorkflowRef checks a reference to an admitted definition.
func ValidateWorkflowRef(r WorkflowRef) error {
	if r.Version != WorkflowVersion {
		return fmt.Errorf("contract: workflow ref version %d is not supported; this build understands %d",
			r.Version, WorkflowVersion)
	}
	if err := ValidateDigest(r.SHA256); err != nil {
		return fmt.Errorf("contract: workflow ref sha256: %w", err)
	}
	return nil
}

// DecodeWorkflowDefinition parses a definition, refusing unknown fields.
func DecodeWorkflowDefinition(data []byte) (*WorkflowDefinition, error) {
	if len(data) > MaxDefinitionBytes {
		return nil, fmt.Errorf("contract: the workflow definition is %d bytes, over the %d byte limit",
			len(data), MaxDefinitionBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var d WorkflowDefinition
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("contract: decoding workflow definition: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("contract: workflow definition contains trailing content after the JSON object")
	}
	return &d, nil
}

// ValidateInputBinding refuses a run whose input does not cover the keys the
// definition declared.
//
// The check is at admission because an absent key substituted as empty is a task
// that runs with a hole in it, and it produces a plausible-looking result.
func ValidateInputBinding(d *WorkflowDefinition, input map[string]string) error {
	if d == nil {
		return fmt.Errorf("contract: no workflow definition supplied")
	}
	needed := map[string]string{} // key -> first task that needs it
	for _, t := range d.Tasks {
		for _, k := range t.InputKeys {
			if _, ok := needed[k]; !ok {
				needed[k] = t.ID
			}
		}
	}
	var missing []string
	for k, task := range needed {
		v, ok := input[k]
		if !ok || strings.TrimSpace(v) == "" {
			missing = append(missing, fmt.Sprintf("%s (required by %s)", k, task))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("contract: the run input is missing required keys: %s", strings.Join(missing, ", "))
	}
	return nil
}

// WorkflowRequest is the v2 admission envelope for a workflow run.
//
// It is a separate type from Request on purpose. v1 describes one task; v2 names
// a graph and supplies its run input. Keeping them apart means a v1 decoder can
// never accept a v2 document and silently ignore the graph it carries.
type WorkflowRequest struct {
	Version   int       `json:"version"`
	RequestID string    `json:"request_id"`
	Origin    Origin    `json:"origin"`
	Workspace Workspace `json:"workspace"`
	// Definition is supplied on FIRST USE. Exactly one of Definition and Ref must
	// be present; a request that supplies both is ambiguous about which one is
	// authoritative, and one that supplies neither cannot be admitted.
	Definition *WorkflowDefinition `json:"workflow_definition,omitempty"`
	// Ref is supplied on every LATER run that reuses an admitted definition.
	Ref *WorkflowRef `json:"workflow_ref,omitempty"`
	// Input is the run input. It never changes the definition's identity, only the
	// request's.
	Input map[string]string `json:"input"`
	// ChildCapability is an engine-minted token permitting this request to activate
	// a child of RootRequestID. It is empty for a top-level caller's first use, and
	// a caller cannot obtain one: only the engine mints them.
	ChildCapability string `json:"child_capability,omitempty"`
	// RootRequestID names the admitted root this request is a child of, when it is
	// one. Empty for a root submission.
	RootRequestID string `json:"root_request_id,omitempty"`
	// Constraints, Limits and Verification apply to the ROOT run. Per-task
	// constraints live in the definition, and a task may only ever be NARROWER
	// than its root.
	Constraints  Constraints        `json:"constraints"`
	Verification []VerificationItem `json:"verification,omitempty"`
	Limits       Limits             `json:"limits,omitempty"`
}

// DecodeWorkflowRequest parses a v2 workflow request, refusing unknown fields.
//
// Version is checked here rather than at use, because a document whose version
// this build does not understand may carry a safety field it would not enforce.
func DecodeWorkflowRequest(data []byte) (*WorkflowRequest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r WorkflowRequest
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("contract: decoding workflow request: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("contract: workflow request contains trailing content after the JSON object")
	}
	if r.Version != RequestVersion2 {
		return nil, fmt.Errorf("contract: workflow request version %d is not supported; this build understands %d",
			r.Version, RequestVersion2)
	}
	if err := ValidateRequestID(r.RequestID); err != nil {
		return nil, fmt.Errorf("contract: workflow request_id: %w", err)
	}
	if (r.Definition == nil) == (r.Ref == nil) {
		return nil, fmt.Errorf("contract: a workflow request must carry exactly one of " +
			"workflow_definition (first use) or workflow_ref (later use)")
	}
	if r.Ref != nil {
		if err := ValidateWorkflowRef(*r.Ref); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

// CanonicalSHA256Workflow returns the identity of a workflow REQUEST.
//
// It deliberately does NOT include the definition body. Two runs of one definition
// with different inputs share the definition identity and differ here, which is
// exactly the separation the contract needs: reuse is by definition, distinctness
// is by request.
func CanonicalSHA256Workflow(r *WorkflowRequest, definitionSHA, inputDigest, policyDigest string) (string, error) {
	if r == nil {
		return "", fmt.Errorf("contract: cannot hash a nil workflow request")
	}
	if err := ValidateDigest(definitionSHA); err != nil {
		return "", fmt.Errorf("contract: workflow definition digest: %w", err)
	}
	// A stable projection: the definition body is replaced by its digest, and the
	// input map by its own digest, so the envelope is what gets hashed.
	projection := map[string]any{
		"version":      r.Version,
		"request_id":   r.RequestID,
		"origin":       r.Origin,
		"workspace":    r.Workspace,
		"definition":   definitionSHA,
		"input":        inputDigest,
		"policy":       policyDigest,
		"constraints":  r.Constraints,
		"verification": r.Verification,
		"limits":       r.Limits,
	}
	raw, err := json.Marshal(projection)
	if err != nil {
		return "", fmt.Errorf("contract: encoding workflow request: %w", err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return "", fmt.Errorf("contract: normalising workflow request: %w", err)
	}
	canonical, err := json.Marshal(generic)
	if err != nil {
		return "", fmt.Errorf("contract: encoding canonical workflow request: %w", err)
	}
	h := sha256.New()
	h.Write([]byte("agent-engine/workflow-request/v2\x00"))
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// InputDigest returns the identity of a run input.
func InputDigest(input map[string]string) (string, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("contract: encoding workflow input: %w", err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return "", fmt.Errorf("contract: normalising workflow input: %w", err)
	}
	canonical, err := json.Marshal(generic)
	if err != nil {
		return "", fmt.Errorf("contract: encoding canonical workflow input: %w", err)
	}
	h := sha256.New()
	h.Write([]byte("agent-engine/workflow-input/v1\x00"))
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil)), nil
}
