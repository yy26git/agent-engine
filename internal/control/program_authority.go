package control

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/emjin/agent-engine/contract"
)

// ProgramAuthorityGrant is a local, operator-provisioned pin for one immutable
// delegated-program definition. It is intentionally separate from the
// submitted definition: possessing an AuthorityRef string is not proof of
// authority; the exact definition digest and expiry must match CONTROL.toml.
type ProgramAuthorityGrant struct {
	AuthorityRef     string
	ProgramID        string
	DefinitionSHA256 string
	ExpiresAt        string
}

func loadProgramAuthorities(doc map[string]any, cfg *Config) error {
	raw, present := doc["program_authorities"]
	if !present {
		return nil
	}
	table, ok := raw.(map[string]any)
	if !ok {
		return cfgErr("program_authorities", "expected authority-keyed tables")
	}
	cfg.ProgramAuthorities = make(map[string]ProgramAuthorityGrant, len(table))
	for ref, rawGrant := range table {
		field := "program_authorities." + ref
		grantTable, ok := rawGrant.(map[string]any)
		if !ok {
			return cfgErr(field, "expected a grant table")
		}
		for key := range grantTable {
			switch key {
			case "program_id", "definition_sha256", "expires_at":
			default:
				return cfgErr(field+"."+key, "unknown program authority setting")
			}
		}
		grant := ProgramAuthorityGrant{
			AuthorityRef:     ref,
			ProgramID:        stringField(grantTable, "program_id"),
			DefinitionSHA256: strings.TrimSpace(stringField(grantTable, "definition_sha256")),
			ExpiresAt:        stringField(grantTable, "expires_at"),
		}
		if strings.TrimSpace(ref) == "" {
			return cfgErr("program_authorities", "authority reference must not be empty")
		}
		if err := contract.ValidateRequestID(grant.ProgramID); err != nil {
			return cfgErr(field+".program_id", "%s", err)
		}
		if !validLowerSHA256(grant.DefinitionSHA256) {
			return cfgErr(field+".definition_sha256", "must be a lowercase SHA-256 digest")
		}
		if _, err := time.Parse(time.RFC3339, grant.ExpiresAt); err != nil {
			return cfgErr(field+".expires_at", "RFC3339 expiry required")
		}
		cfg.ProgramAuthorities[ref] = grant
	}
	return nil
}

func validLowerSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// VerifyProgramAuthority checks a caller-presented definition against the
// locally configured digest and expiry. The returned digest is match evidence,
// not a plan acceptance receipt and not permission to execute a child.
func (s *Store) VerifyProgramAuthority(program *contract.ProgramDefinition) (string, error) {
	if err := contract.ValidateProgramDefinition(program); err != nil {
		return "", err
	}
	grant, ok := s.cfg.ProgramAuthorities[program.AuthorityRef]
	if !ok {
		return "", fmt.Errorf("agent-engine: program authority %q is not configured locally", program.AuthorityRef)
	}
	if grant.ProgramID != program.ProgramID {
		return "", fmt.Errorf("agent-engine: local program authority is for %q, not %q", grant.ProgramID, program.ProgramID)
	}
	expires, err := time.Parse(time.RFC3339, grant.ExpiresAt)
	if err != nil || !s.now().Before(expires) {
		return "", fmt.Errorf("agent-engine: local program authority is expired or has an invalid expiry")
	}
	digest, err := program.ProgramDigest()
	if err != nil {
		return "", err
	}
	if digest != grant.DefinitionSHA256 {
		return "", fmt.Errorf("agent-engine: program definition digest does not match the local operator grant")
	}
	return digest, nil
}
