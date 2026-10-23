package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/emjin/agent-engine/contract"
)

// InstructionFile is one measured file in an admitted definition bundle.
type InstructionFile = contract.InstructionFile

// Immutable workflow admission.
//
// Admission is the moment a caller's words become durable work, so everything
// inconvenient happens here, once, while nothing has started:
//
//   1. The definition's STRUCTURE is checked (cycles, missing dependencies,
//      duplicate ids, both-or-neither instruction sources).
//   2. The definition's REFERENCES are checked against the operator's own
//      registry — profiles and checks this deployment actually has.
//   3. The instruction files are READ and MEASURED. The caller's claimed digest
//      is never believed; if the caller supplies one it must equal what was read.
//   4. The definition identity is computed over canonical JSON plus the file
//      manifest, and the bundle is published ATOMICALLY under that identity.
//   5. The root request, its immutable WORKFLOW.json and every child request are
//      published under one admission lock, or nothing is.
//
// The ordering matters most at step 3. A digest a caller supplies is a claim; a
// digest this layer measures is evidence, and a run whose instruction file changed
// between approval and launch must not be able to present the old approval.

// MaxWorkflowAdmissionBytes bounds one workflow request document.
const MaxWorkflowAdmissionBytes = 4 << 20

// WorkflowsDirName is where admitted definition bundles live.
const WorkflowsDirName = "WORKFLOWS"

// WorkflowBundleFile is the canonical definition document inside a bundle.
const WorkflowBundleFile = "DEFINITION.json"

// WorkflowFilesDirName is the instruction file directory inside a bundle.
const WorkflowFilesDirName = "FILES"

// WorkflowStateFile is the immutable admitted record inside a root request.
const WorkflowStateFile = "WORKFLOW.json"

// AdmittedWorkflow is what a root request records about its graph.
//
// It is written once and never edited. Everything mutable about the run — the
// launch epoch, cancellation, the committed outcome — lives in STATE.json instead,
// so this record can be trusted as "what was admitted" without reading the
// mutable record that describes what happened to it.
type AdmittedWorkflow struct {
	// Version is the definition schema version.
	Version int `json:"version"`
	// DefinitionSHA256 is the identity of the admitted definition.
	DefinitionSHA256 string `json:"definition_sha256"`
	// Name is the definition's human name, for display only.
	Name string `json:"name"`
	// InputDigest is the identity of this run's input.
	InputDigest string `json:"input_sha256"`
	// PolicyDigest pins the operator policy in force at admission.
	PolicyDigest string `json:"policy_sha256"`
	// RequestSHA256 is the root request's identity.
	RequestSHA256 string `json:"request_sha256"`
	// Children is the immutable child map: task id to derived request ID.
	Children map[string]string `json:"children"`
	// ChildOrder preserves the definition's declared order, so a reader sees the
	// graph as written rather than in map order.
	ChildOrder []string `json:"child_order"`
	// Instructions records the measured instruction file digests.
	Instructions []InstructionFile `json:"instructions,omitempty"`
	// AdmittedAt is when this record was written.
	AdmittedAt string `json:"admitted_at"`
}

// WorkflowAdmission is the outcome of admitting a workflow run.
type WorkflowAdmission struct {
	// RequestID is the root request ID.
	RequestID string
	// DefinitionSHA256 is the admitted definition's identity.
	DefinitionSHA256 string
	// RequestSHA256 is the root request's identity.
	RequestSHA256 string
	// InputDigest is this run's input identity.
	InputDigest string
	// Children is the immutable child map.
	Children map[string]string
	// Duplicate is true when an identical request was already admitted.
	Duplicate bool
	// Ref is what a caller keeps to reuse this definition.
	Ref contract.WorkflowRef
}

// AdmitWorkflow admits a workflow run and publishes its bundle and children.
//
// The whole sequence happens under the store lock, and the bundle is written to a
// temporary directory and published by rename. A crash therefore leaves either a
// complete admitted run or nothing recognisable as one — never a root request
// whose children are half written.
func (s *Store) AdmitWorkflow(req *contract.WorkflowRequest) (WorkflowAdmission, error) {
	var out WorkflowAdmission
	if req == nil {
		return out, fmt.Errorf("agent-engine: no workflow request supplied")
	}
	if len(req.Input) > 0 {
		raw, err := json.Marshal(req.Input)
		if err != nil {
			return out, fmt.Errorf("agent-engine: the run input cannot be encoded: %w", err)
		}
		if len(raw) > contract.MaxInputBytes {
			return out, fmt.Errorf("agent-engine: the run input is %d bytes, over the %d byte limit",
				len(raw), contract.MaxInputBytes)
		}
	}

	// The exactly-one rule is checked HERE, not left to the caller's decoder.
	// AdmitWorkflow is reachable from tests, from a future in-process caller and
	// from the CLI, and a rule enforced only at the edge is a rule that is one
	// call path away from a nil dereference.
	if (req.Definition == nil) == (req.Ref == nil) {
		return out, fmt.Errorf("agent-engine: a workflow request must carry exactly one of " +
			"workflow_definition (first use) or workflow_ref (later use)")
	}
	if req.Ref != nil {
		if err := contract.ValidateWorkflowRef(*req.Ref); err != nil {
			return out, fmt.Errorf("agent-engine: %w", err)
		}
	}

	def, instructions, defSHA, err := s.resolveDefinition(req)
	if err != nil {
		return out, err
	}
	// Every reference the graph makes must exist in THIS deployment's registry.
	if err := s.validateGraphReferences(def, req.Workspace.Path); err != nil {
		return out, err
	}
	if err := contract.ValidateInputBinding(def, req.Input); err != nil {
		return out, err
	}

	inputDigest, err := contract.InputDigest(req.Input)
	if err != nil {
		return out, err
	}
	// A task may only ever be narrower than its root. A graph that tries to widen
	// the root's grant is refused before anything is written.
	if err := s.validateNoPermissionEscalation(req, def); err != nil {
		return out, err
	}
	// Title collision is checked at admission, not left to the outer scheduler.
	// A consumer that trims and lowercases titles would otherwise merge two tasks
	// this layer believes are distinct, and the merge would be invisible here.
	if err := s.validateTitleUniqueness(def); err != nil {
		return out, err
	}
	// A nested submission requires an ENGINE-MINTED capability, not a declared
	// origin. An engine-owned executor may admit a child of an admitted root; it
	// may not acquire a second, unbounded graph.
	if err := s.verifyChildAdmission(req); err != nil {
		return out, err
	}

	reqDigest, err := contract.CanonicalSHA256Workflow(req, defSHA, inputDigest, s.cfg.Digest)
	if err != nil {
		return out, err
	}

	children, order, err := deriveChildren(req.RequestID, defSHA, def)
	if err != nil {
		return out, err
	}

	unlock, err := s.storeLock()
	if err != nil {
		return out, err
	}
	defer unlock()

	// A conflicting ID is refused before the bundle is touched, so a rejected
	// admission leaves no new artifacts behind.
	dir, err := s.RequestDir(req.RequestID)
	if err != nil {
		return out, err
	}
	existing, err := s.loadState(dir)
	if err != nil && !errors.Is(err, ErrNoState) {
		return out, err
	}
	if existing != nil {
		if existing.RequestDigest == reqDigest {
			out.Duplicate = true
			out.RequestID = req.RequestID
			out.DefinitionSHA256 = defSHA
			out.RequestSHA256 = reqDigest
			out.InputDigest = inputDigest
			out.Children = children
			out.Ref = contract.WorkflowRef{Version: contract.WorkflowVersion, SHA256: defSHA}
			return out, nil
		}
		return out, fmt.Errorf("agent-engine: request %s is already admitted with a different definition or input; "+
			"a published identity never changes, so submit a new request id instead", req.RequestID)
	}

	// The bundle is published under the measured definition identity.
	if err := s.publishBundle(defSHA, def, instructions); err != nil {
		return out, err
	}

	now := s.nowRFC3339()
	admitted := AdmittedWorkflow{
		Version:          contract.WorkflowVersion,
		DefinitionSHA256: defSHA,
		Name:             def.Name,
		InputDigest:      inputDigest,
		PolicyDigest:     s.cfg.Digest,
		RequestSHA256:    reqDigest,
		Children:         children,
		ChildOrder:       order,
		Instructions:     instructions,
		AdmittedAt:       now,
	}

	// The root request is admitted first, so a child can never reference a root
	// that does not exist.
	rootReq := reqToV1(req, defSHA, reqDigest, s.cfg.Digest, def)
	rootDir, err := s.RequestDir(rootReq.RequestID)
	if err != nil {
		return out, err
	}
	if _, err := s.admitLocked(&rootReq, rootDir, reqDigest); err != nil {
		return out, err
	}
	if err := s.writeWorkflowRecord(req.RequestID, admitted); err != nil {
		return out, err
	}
	// The child map is pinned into root state, so a reader of STATE.json alone
	// knows which children this root owns.
	rootStateDir, err := s.RequestDir(req.RequestID)
	if err != nil {
		return out, err
	}
	if _, err := s.updateCommittedLocked(req.RequestID, rootStateDir, func(st *State) error {
		st.WorkflowDefinitionSHA = defSHA
		st.WorkflowInputDigest = inputDigest
		st.WorkflowChildOrder = append([]string(nil), order...)
		st.WorkflowChildren = map[string]string{}
		for k, v := range children {
			st.WorkflowChildren[k] = v
		}
		// The root takes the workspace lease capability for its epoch; children
		// are delegated within it rather than each acquiring their own.
		return s.acquireRootLease(st, req, now)
	}); err != nil {
		return out, err
	}

	// Each child is admitted as an ordinary v1 task request, so every downstream
	// owner (engine, verifier, audit) works unchanged on a child. The whole set
	// goes in under the ONE lock already held, which is what makes "a root and
	// its children exist together or not at all" true rather than aspirational.
	for _, task := range def.Tasks {
		childReq := childRequestFor(req, rootReq.Constraints, def, task, children, reqDigest, s.cfg.Digest)
		childDigest, derr := contract.CanonicalSHA256(&childReq)
		if derr != nil {
			return out, fmt.Errorf("agent-engine: encoding child request %s: %w", childReq.RequestID, derr)
		}
		childDir, derr := s.RequestDir(childReq.RequestID)
		if derr != nil {
			return out, derr
		}
		profiles, perr := s.taskProfiles(task)
		if perr != nil {
			return out, perr
		}
		if _, aerr := s.admitLockedWithProfiles(&childReq, childDir, childDigest, profiles); aerr != nil {
			return out, fmt.Errorf("agent-engine: admitting child %s: %w", childReq.RequestID, aerr)
		}
	}

	out.RequestID = req.RequestID
	out.DefinitionSHA256 = defSHA
	out.RequestSHA256 = reqDigest
	out.InputDigest = inputDigest
	out.Children = children
	out.Ref = contract.WorkflowRef{Version: contract.WorkflowVersion, SHA256: defSHA}
	return out, nil
}

// resolveDefinition returns the definition to admit, its measured instruction
// files, and its identity.
//
// A REF resolves to the already-admitted bundle and its bytes are RE-MEASURED from
// disk. That is what makes a reference immutable: if the bundle were tampered
// with, the recomputed identity would not match the reference, and the run is
// refused rather than executing altered instructions.
func (s *Store) resolveDefinition(req *contract.WorkflowRequest) (*contract.WorkflowDefinition, []InstructionFile, string, error) {
	if req.Definition != nil {
		def := req.Definition
		if err := contract.ValidateWorkflowDefinition(def); err != nil {
			return nil, nil, "", fmt.Errorf("agent-engine: %w", err)
		}
		files, err := s.measureInlineInstructions(def)
		if err != nil {
			return nil, nil, "", err
		}
		// A caller MAY claim a digest. It is checked against what was measured, so
		// a claim cannot substitute for evidence — but a matching claim proves the
		// caller intended exactly these bytes.
		if want := claimedDefinitionDigest(req, def); want != "" {
			sha, err := contract.DefinitionIdentity(def, files)
			if err != nil {
				return nil, nil, "", err
			}
			if want != sha {
				return nil, nil, "", fmt.Errorf("agent-engine: the submitted definition hashes to %s but the "+
					"caller claimed %s; the bytes are authoritative, not the claim", sha, want)
			}
		}
		sha, err := contract.DefinitionIdentity(def, files)
		if err != nil {
			return nil, nil, "", err
		}
		return def, files, sha, nil
	}

	// A ref must resolve to a bundle already on disk.
	ref := *req.Ref
	def, files, sha, err := s.loadBundle(ref.SHA256)
	if err != nil {
		return nil, nil, "", err
	}
	if sha != ref.SHA256 {
		return nil, nil, "", fmt.Errorf("agent-engine: the stored bundle for %s now hashes to %s; "+
			"an admitted definition's bytes must not change under a live reference", ref.SHA256, sha)
	}
	return def, files, sha, nil
}

// claimedDefinitionDigest returns a caller-claimed digest if one was supplied.
func claimedDefinitionDigest(req *contract.WorkflowRequest, def *contract.WorkflowDefinition) string {
	// The envelope's optional ref is the only place a caller can pre-declare a
	// digest alongside a first-use definition; an empty string means "no claim".
	if req.Ref != nil {
		return req.Ref.SHA256
	}
	_ = def
	return ""
}

// measureInlineInstructions hashes the instruction bodies a definition carries.
//
// Only INLINE bodies can be measured here. A definition naming instruction FILES
// must be admitted from a bundle that already contains them, because a
// bundle-relative path is not a host path and this layer will not read a
// caller-chosen one.
func (s *Store) measureInlineInstructions(def *contract.WorkflowDefinition) ([]InstructionFile, error) {
	var files []InstructionFile
	for _, t := range def.Tasks {
		if strings.TrimSpace(t.InstructionFile) != "" {
			return nil, fmt.Errorf("agent-engine: task %q names instruction_file %q, but a first-use definition "+
				"carries inline instructions only; a file-backed instruction belongs to an admitted bundle",
				t.ID, t.InstructionFile)
		}
		body := t.Instruction
		if len(body) > contract.MaxInstructionBytes {
			return nil, fmt.Errorf("agent-engine: task %q has a %d byte instruction, over the %d byte limit",
				t.ID, len(body), contract.MaxInstructionBytes)
		}
		files = append(files, InstructionFile{
			Path:   "tasks/" + t.ID + ".md",
			SHA256: sha256Hex([]byte(body)),
			Bytes:  len(body),
		})
	}
	return files, nil
}

func (s *Store) taskProfiles(t contract.WorkflowTask) (map[ProfileRole]ProfileRef, error) {
	out := map[ProfileRole]ProfileRef{}
	for _, slot := range []struct {
		role ProfileRole
		name string
	}{{RoleWorker, t.ExecutorProfile}, {RoleReviewer, t.ReviewerProfile}, {RoleCheckpoint, t.CheckpointProfile}, {RoleDiagnostic, t.DiagnosticProfile}} {
		name, requiredRole := slot.name, slot.role
		if name == "" {
			name = t.ReviewerProfile
			requiredRole = RoleReviewer
		}
		p, err := s.cfg.ResolveProfile(name, requiredRole)
		if err != nil {
			return nil, fmt.Errorf("task %q %s profile: %w", t.ID, slot.role, err)
		}
		out[slot.role] = p.Ref()
	}
	return out, nil
}

// loadBundle reads an admitted bundle and re-measures its identity.
func (s *Store) loadBundle(sha string) (*contract.WorkflowDefinition, []InstructionFile, string, error) {
	if err := contract.ValidateDigest(sha); err != nil {
		return nil, nil, "", fmt.Errorf("agent-engine: workflow ref sha256: %w", err)
	}
	rel := filepath.Join(WorkflowsDirName, sha)
	defPath := filepath.Join(s.cfg.StoreRoot, rel, WorkflowBundleFile)
	raw, err := os.ReadFile(defPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("agent-engine: the workflow definition %s has not been admitted "+
			"into this store, or its bundle is unreadable: %w", sha, err)
	}
	def, err := contract.DecodeWorkflowDefinition(raw)
	if err != nil {
		return nil, nil, "", err
	}
	if err := contract.ValidateWorkflowDefinition(def); err != nil {
		return nil, nil, "", fmt.Errorf("agent-engine: the stored definition for %s is no longer valid: %w", sha, err)
	}
	// Instruction files are re-read and re-measured, so a tampered bundle cannot
	// be reused under its original identity. The walk is RECURSIVE and records
	// paths relative to the FILES directory, because the manifest paths are
	// bundle-relative and nested ("tasks/<id>.md"). A non-recursive read of FILES
	// would find only the "tasks" directory itself and silently measure nothing,
	// which is the failure that would make a tampered bundle look intact.
	filesDir := filepath.Join(s.cfg.StoreRoot, rel, WorkflowFilesDirName)
	var files []InstructionFile
	walkErr := filepath.WalkDir(filesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		relPath, rerr2 := filepath.Rel(filesDir, p)
		if rerr2 != nil {
			return rerr2
		}
		files = append(files, InstructionFile{
			Path:   filepath.ToSlash(relPath),
			SHA256: sha256Hex(data),
			Bytes:  len(data),
		})
		return nil
	})
	if walkErr != nil {
		// A definition whose tasks all carry inline instructions has instruction
		// files, so a missing FILES directory is a corrupt bundle, not an empty
		// one. It is refused rather than treated as zero files.
		return nil, nil, "", fmt.Errorf("agent-engine: the bundle for %s has unreadable instruction files: %w", sha, walkErr)
	}
	if len(files) == 0 {
		return nil, nil, "", fmt.Errorf("agent-engine: the bundle for %s contains no instruction files, but its "+
			"definition declares %d task(s); an empty file set is a corrupt bundle", sha, len(def.Tasks))
	}
	measured, err := contract.DefinitionIdentity(def, files)
	if err != nil {
		return nil, nil, "", err
	}
	return def, files, measured, nil
}

// publishBundle writes a definition bundle under its measured identity.
//
// The write goes to a temporary directory and is published by rename, so a bundle
// either exists completely or does not exist. Publishing into a directory that is
// already present is treated as success, because identity means the bytes are the
// same bytes and rewriting them would be pointless.
func (s *Store) publishBundle(sha string, def *contract.WorkflowDefinition, files []InstructionFile) error {
	rel := filepath.Join(WorkflowsDirName, sha)
	dir := filepath.Join(s.cfg.StoreRoot, rel)
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	staging, err := os.MkdirTemp(s.cfg.StoreRoot, ".workflow-staging-")
	if err != nil {
		return fmt.Errorf("agent-engine: staging the workflow bundle: %w", err)
	}
	// A staging directory left behind by a crash is residue, not state. It is
	// removed on the failure path below; on the success path rename consumes it.
	defer func() {
		if staging != "" {
			_ = os.RemoveAll(staging)
		}
	}()

	defBytes, err := json.MarshalIndent(def, "", "  ")
	if err != nil {
		return fmt.Errorf("agent-engine: encoding the workflow definition: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, WorkflowBundleFile), append(defBytes, '\n'), 0o444); err != nil {
		return fmt.Errorf("agent-engine: writing the workflow definition: %w", err)
	}
	filesDir := filepath.Join(staging, WorkflowFilesDirName)
	if err := os.MkdirAll(filepath.Join(filesDir, "tasks"), 0o755); err != nil {
		return fmt.Errorf("agent-engine: staging instruction files: %w", err)
	}
	for i, f := range files {
		// The body is the task's inline instruction, matched by the same path the
		// manifest recorded.
		body := ""
		for _, t := range def.Tasks {
			if f.Path == "tasks/"+t.ID+".md" {
				body = t.Instruction
				break
			}
		}
		if got := sha256Hex([]byte(body)); got != f.SHA256 {
			return fmt.Errorf("agent-engine: instruction for %s hashes to %s but the manifest recorded %s",
				f.Path, got, f.SHA256)
		}
		// The manifest path is bundle-relative ("tasks/<id>.md"), and the bundle
		// root already IS the FILES directory, so the path is joined whole rather
		// than having its prefix trimmed.
		target := filepath.Join(filesDir, filepath.FromSlash(f.Path))
		if err := os.WriteFile(target, []byte(body), 0o444); err != nil {
			return fmt.Errorf("agent-engine: writing instruction %s: %w", f.Path, err)
		}
		_ = i
	}
	manifest := contract.DefinitionManifest{Schema: contract.WorkflowVersion, Files: files}
	manBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("agent-engine: encoding the definition manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "MANIFEST.json"), append(manBytes, '\n'), 0o444); err != nil {
		return fmt.Errorf("agent-engine: writing the definition manifest: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("agent-engine: creating the workflow directory: %w", err)
	}
	if err := os.Rename(staging, dir); err != nil {
		// A concurrent publisher winning the race is success: identity means the
		// same bytes, so whichever bundle landed first is the correct one.
		if _, statErr := os.Stat(dir); statErr == nil {
			return nil
		}
		return fmt.Errorf("agent-engine: publishing the workflow bundle: %w", err)
	}
	staging = ""
	return nil
}

// writeWorkflowRecord publishes the root's immutable admitted record.
func (s *Store) writeWorkflowRecord(requestID string, admitted AdmittedWorkflow) error {
	rel := filepath.Join(RequestsDirName, requestID, WorkflowStateFile)
	if err := writeJSONDurable(s.cfg.StoreRoot, rel, admitted, 0o444); err != nil {
		return fmt.Errorf("agent-engine: writing the admitted workflow record: %w", err)
	}
	return nil
}

// validateGraphReferences checks every profile and check a definition names
// against this deployment's own registry.
//
// A definition that references a profile this store does not have is refused at
// admission rather than at launch, because admitting it would create child
// requests that can never run.
func (s *Store) validateGraphReferences(def *contract.WorkflowDefinition, workspace string) error {
	for _, t := range def.Tasks {
		exec, err := s.cfg.ResolveProfile(t.ExecutorProfile, RoleWorker)
		if err != nil {
			return fmt.Errorf("agent-engine: task %q executor profile %q: %w", t.ID, t.ExecutorProfile, err)
		}
		if _, err := s.cfg.ResolveProfile(t.ReviewerProfile, RoleReviewer); err != nil {
			return fmt.Errorf("agent-engine: task %q reviewer profile %q: %w", t.ID, t.ReviewerProfile, err)
		}
		if exec.Role == RoleWorker && t.ReviewerProfile == exec.Name {
			return fmt.Errorf("agent-engine: task %q names profile %q for both roles", t.ID, exec.Name)
		}
		profiles, err := s.taskProfiles(t)
		if err != nil {
			return err
		}
		for _, ref := range profiles {
			p := ref.Profile()
			if p.Tool == "codex" && p.ReadOnly {
				if _, err := s.cfg.VerifyConfinementReceipt(p, workspace); err != nil {
					return err
				}
			}
		}
		for _, ck := range t.CheckProfiles {
			if _, ok := s.cfg.Checks[ck]; !ok {
				return fmt.Errorf("agent-engine: task %q names check profile %q, which this deployment's "+
					"registry does not contain", t.ID, ck)
			}
		}
		for _, o := range t.Obligations {
			for _, ck := range o.RequiredChecks {
				if _, ok := s.cfg.Checks[ck]; !ok {
					return fmt.Errorf("agent-engine: task %q obligation %q requires check %q, which this "+
						"deployment's registry does not contain", t.ID, o.ID, ck)
				}
			}
		}
	}
	return nil
}

// validateNoPermissionEscalation refuses a graph that widens the root's grant.
//
// A child may be narrower than its root. It may never be wider: a definition that
// asks for more than the caller was granted is a way to obtain capability through
// indirection, and the indirection is the part that usually goes unreviewed.
func (s *Store) validateNoPermissionEscalation(req *contract.WorkflowRequest, def *contract.WorkflowDefinition) error {
	rootAllowsChanges := req.Constraints.AllowCodeChanges
	for _, t := range def.Tasks {
		taskAllowsChanges := t.Operation.CodeChangeAllowed()
		if taskAllowsChanges && !rootAllowsChanges {
			return fmt.Errorf("agent-engine: task %q has operation %s, which may change code, but the root "+
				"request does not grant code changes; a child may only be narrower than its root",
				t.ID, t.Operation)
		}
	}
	return nil
}

// deriveChildren produces the immutable child map and preserves declared order.
func deriveChildren(rootID, defSHA string, def *contract.WorkflowDefinition) (map[string]string, []string, error) {
	children := make(map[string]string, len(def.Tasks))
	order := make([]string, 0, len(def.Tasks))
	for _, t := range def.Tasks {
		id := contract.ChildRequestID(rootID, defSHA, t.ID)
		if prev, dup := children[t.ID]; dup {
			return nil, nil, fmt.Errorf("agent-engine: task %q derives a child id that collides with %q", t.ID, prev)
		}
		// The derived ID is itself a request ID, so it must satisfy the same
		// charset the store already enforces.
		if err := contract.ValidateRequestID(id); err != nil {
			return nil, nil, fmt.Errorf("agent-engine: task %q derives an invalid child request id %q: %w", t.ID, id, err)
		}
		children[t.ID] = id
		order = append(order, t.ID)
	}
	return children, order, nil
}

// reqToV1 projects a workflow root onto an ordinary admitted v1 request.
//
// The root is itself a supervised task, so it goes through the same admission path
// as any other. That is what keeps one admission code path rather than two that
// drift apart.
func reqToV1(req *contract.WorkflowRequest, defSHA, reqDigest, policyDigest string, def *contract.WorkflowDefinition) contract.Request {
	objective := fmt.Sprintf("Coordinate the admitted workflow %s (%s)", req.RequestID, defSHA[:12])
	// The root's operation is the MOST PERMISSIVE operation in the graph, and its
	// constraint set follows from that.
	//
	// This is not a convenience. A root pinned to VERIFY could not carry
	// allow_code_changes — v1 refuses that combination — which would make every
	// REPAIR task in the graph unadmittable. Conversely a root claiming to repair
	// while the graph only investigates would overstate the grant. Taking the
	// graph's own maximum keeps the root's authority exactly equal to what its
	// children may do, so "a child is never wider than its root" holds by
	// construction rather than by coincidence.
	rootOp := contract.OpInvestigate
	for _, t := range def.Tasks {
		if t.Operation.CodeChangeAllowed() {
			rootOp = contract.OpRepair
			break
		}
		if t.Operation == contract.OpVerify {
			rootOp = contract.OpVerify
		}
	}
	constraints := req.Constraints
	constraints.AllowCodeChanges = req.Constraints.AllowCodeChanges && rootOp.CodeChangeAllowed()
	// A workflow's objective is the graph itself, so its obligations are the
	// graph-level ones: the run must not claim success unless every required
	// child result says so, which S5 enforces.
	return contract.Request{
		Version:      contract.Version,
		RequestID:    req.RequestID,
		Origin:       req.Origin,
		Operation:    rootOp,
		Workspace:    req.Workspace,
		Objective:    objective,
		Obligations:  []contract.Obligation{{ID: "W-1", Criterion: "every required child result is committed PASS"}},
		Constraints:  constraints,
		Verification: req.Verification,
		Limits:       req.Limits,
	}
}

// childRequestFor projects one graph task onto an admitted v1 child request.
//
// The child is a complete, independently supervised engineering task. It gets its
// own obligations, checks and limits, and its objective carries the run input
// values its task declared, so a task's instruction and the input it was given
// arrive in the same durable place.
func childRequestFor(root *contract.WorkflowRequest, rootConstraints contract.Constraints, def *contract.WorkflowDefinition, t contract.WorkflowTask, children map[string]string, parentDigest, policyDigest string) contract.Request {
	objective := t.Objective
	if t.Instruction != "" {
		objective = t.Instruction
	}
	// Declared input keys are substituted into the objective. A key the definition
	// declared but the run input lacked was already refused at admission, so this
	// cannot silently produce an empty value.
	if len(t.InputKeys) > 0 {
		var parts []string
		for _, k := range t.InputKeys {
			parts = append(parts, fmt.Sprintf("%s=%s", k, root.Input[k]))
		}
		objective = objective + "\n\nInput: " + strings.Join(parts, " ")
	}
	obligations := make([]contract.Obligation, 0, len(t.Obligations))
	for _, o := range t.Obligations {
		obligations = append(obligations, contract.Obligation{
			ID:             o.ID,
			Criterion:      o.Criterion,
			RequiredChecks: o.RequiredChecks,
		})
	}
	if len(obligations) == 0 {
		obligations = []contract.Obligation{{
			ID:        "C-1",
			Criterion: fmt.Sprintf("task %q satisfies its objective", t.ID),
		}}
	}
	verification := make([]contract.VerificationItem, 0, len(t.CheckProfiles))
	for _, ck := range t.CheckProfiles {
		verification = append(verification, contract.VerificationItem{CheckID: ck})
	}
	return contract.Request{
		Version:      contract.Version,
		RequestID:    children[t.ID],
		Origin:       root.Origin,
		Operation:    t.Operation,
		Workspace:    root.Workspace,
		Objective:    objective,
		Obligations:  obligations,
		Constraints:  rootConstraints,
		Verification: verification,
		Limits:       root.Limits,
	}
}

// acquireRootLease takes the root's workspace ownership for its epoch.
//
// Children do not each acquire a competing lease: the root holds it once and
// delegates within its epoch. Two roots on one workspace would serialise, which is
// the property that makes a single-writer claim true rather than aspirational.
func (s *Store) acquireRootLease(st *State, req *contract.WorkflowRequest, now string) error {
	// The lease root is the workspace itself; per-child delegation is recorded as
	// part of the admitted child map, which is immutable.
	st.WorkspaceLeaseRoot = req.Workspace.Path
	st.WorkspaceLeaseEpoch = st.WorkspaceLeaseEpoch + 1
	return st.transition(StageReady, "workflow admitted; the root holds the workspace for epoch "+
		itoa(st.WorkspaceLeaseEpoch), now)
}

// verifyChildAdmission refuses a request that is not the engine activating a child
// of an already-admitted root.
//
// The rule is unchanged from the origin heuristic, but the EVIDENCE is not: a
// caller may declare any origin it likes, whereas the capability is a token the
// store minted and holds the only copy of. A request presenting no capability is
// a first-use root submission and is allowed; one presenting a capability must
// present the store's token for the root it claims.
func (s *Store) verifyChildAdmission(req *contract.WorkflowRequest) error {
	if strings.TrimSpace(req.ChildCapability) == "" {
		// No capability presented: this is a top-level caller submitting a graph,
		// which is the supported path. A caller cannot get a capability by asking
		// for one; only the engine mints them.
		return nil
	}
	if strings.TrimSpace(req.RootRequestID) == "" {
		return fmt.Errorf("%w: a child capability was presented without naming the root it activates",
			ErrCapabilityInvalid)
	}
	st, err := s.State(req.RootRequestID)
	if err != nil {
		return fmt.Errorf("%w: the root %s named by this child submission is not an admitted request: %v",
			ErrCapabilityInvalid, req.RootRequestID, err)
	}
	now := s.now()
	if err := st.ChildCapability.Valid(req.ChildCapability, ScopeChildActivation, req.RootRequestID,
		now.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	// The capability is bound to one definition, so it cannot be reused to activate
	// a different graph.
	if st.ChildCapability.DefinitionSHA != "" && req.Definition != nil {
		_, _, digest, derr := s.resolveDefinition(req)
		if derr != nil {
			return derr
		}
		if !strings.EqualFold(st.ChildCapability.DefinitionSHA, digest) {
			return fmt.Errorf("%w: the capability authorises definition %s but the submission is %s",
				ErrCapabilityInvalid, st.ChildCapability.DefinitionSHA, digest)
		}
	}
	return nil
}

// validateTitleUniqueness refuses two tasks whose ids collide once an outer
// scheduler normalises them.
//
// OMA derives its dependency graph from titles and normalises them by trimming and
// lowercasing. Two ids that normalise to the same title would be merged there
// while this layer counted them as two tasks, so the collision is caught here where
// it can still be refused.
func (s *Store) validateTitleUniqueness(def *contract.WorkflowDefinition) error {
	seen := make(map[string]string, len(def.Tasks))
	for _, t := range def.Tasks {
		normalised := strings.ToLower(strings.TrimSpace(t.ID))
		if prev, dup := seen[normalised]; dup {
			return fmt.Errorf("agent-engine: tasks %q and %q normalise to the same title %q; "+
				"an outer scheduler would merge them into one task", prev, t.ID, normalised)
		}
		seen[normalised] = t.ID
	}
	return nil
}
