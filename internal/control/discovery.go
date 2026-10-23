package control

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

const deploymentFile = "DEPLOYMENT.json"

type Deployment struct {
	Schema int    `json:"schema_version"`
	ID     string `json:"deployment_id"`
}

// Deployment reads identity without creating configuration or processes.
func (s *Store) Deployment() (Deployment, error) {
	var d Deployment
	if err := readJSONStrict(filepath.Join(s.cfg.StoreRoot, deploymentFile), &d); err != nil {
		return d, err
	}
	b, err := hex.DecodeString(d.ID)
	if d.Schema != 1 || err != nil || len(b) != 16 {
		return d, fmt.Errorf("invalid deployment identity")
	}
	return d, nil
}

// InitializeDeployment is an explicit operator action, independent of admission.
func (s *Store) InitializeDeployment() (Deployment, error) {
	unlock, err := s.storeLock()
	if err != nil {
		return Deployment{}, err
	}
	defer unlock()
	d, err := s.Deployment()
	if err == nil {
		return d, nil
	}
	if !os.IsNotExist(err) {
		return d, err
	}
	var bytes [16]byte
	if _, err = rand.Read(bytes[:]); err != nil {
		return d, err
	}
	d = Deployment{Schema: 1, ID: hex.EncodeToString(bytes[:])}
	err = writeJSONDurable(s.cfg.StoreRoot, deploymentFile, d, 0600)
	return d, err
}

type ProfileSelection struct {
	Name                 string      `json:"profile"`
	Role                 ProfileRole `json:"role"`
	CLI                  string      `json:"cli"`
	Transport            Transport   `json:"transport"`
	Provider             string      `json:"provider,omitempty"`
	Model                string      `json:"model,omitempty"`
	Effort               string      `json:"effort,omitempty"`
	ConfigurationValid   bool        `json:"configuration_valid"`
	Reason               string      `json:"reason,omitempty"`
	ProviderAvailability string      `json:"provider_availability"`
}

type FlowDiscovery struct {
	Name                 string                          `json:"name"`
	Phases               []EpisodePhase                  `json:"phases"`
	Roles                map[string]ProfileSelection     `json:"roles"`
	RequiredChecks       []string                        `json:"required_checks"`
	PlanAcceptance       string                          `json:"plan_acceptance"`
	ReadOnly             bool                            `json:"read_only"`
	RepairEnabled        bool                            `json:"repair_enabled"`
	RejectionDisposition string                          `json:"rejection_disposition"`
	ReturnEdges          map[EpisodePhase][]EpisodePhase `json:"return_edges,omitempty"`
}

type Catalogue struct {
	Schema        int             `json:"schema_version"`
	DeploymentID  string          `json:"deployment_id,omitempty"`
	SetupRequired bool            `json:"setup_required"`
	PolicyDigest  string          `json:"policy_digest"`
	Provenance    string          `json:"provenance"`
	Flows         []FlowDiscovery `json:"flows"`
}

// Discover projects only approved configuration; it never exposes launch commands
// or environments, and resolving a profile is not a provider availability probe.
func (s *Store) Discover(allowed map[string]bool) (Catalogue, error) {
	c := Catalogue{Schema: 1, PolicyDigest: s.cfg.Digest, Provenance: "operator-configuration", Flows: []FlowDiscovery{}}
	d, err := s.Deployment()
	if err == nil {
		c.DeploymentID = d.ID
	} else if os.IsNotExist(err) {
		c.SetupRequired = true
	} else {
		return c, err
	}
	for _, name := range s.cfg.EpisodeTemplates.Names() {
		if len(allowed) > 0 && !allowed[name] {
			continue
		}
		tpl, _ := s.cfg.EpisodeTemplates.Lookup(name)
		flow := FlowDiscovery{Name: name, Phases: append([]EpisodePhase(nil), tpl.Phases...), RequiredChecks: append([]string(nil), tpl.RequiredChecks...), PlanAcceptance: "local-operator-exact-digest", ReadOnly: tpl.ReadOnly, RepairEnabled: tpl.repairRoutingEnabled(), RejectionDisposition: tpl.RejectionDisposition(), ReturnEdges: tpl.ReturnEdges, Roles: map[string]ProfileSelection{}}
		for role, profileName := range tpl.Roles {
			p, ok := s.cfg.Profiles[profileName]
			selection := ProfileSelection{Name: profileName, Role: role, ProviderAvailability: "unknown"}
			if ok {
				selection.CLI = p.Tool
				selection.Transport = p.TransportFor()
				selection.Provider = p.Provider
				selection.Model = p.Model
				selection.Effort = p.Effort
			}
			_, e := s.cfg.ResolveProfile(profileName, role)
			selection.ConfigurationValid = e == nil
			if e != nil {
				selection.Reason = e.Error()
			}
			flow.Roles[string(role)] = selection
		}
		c.Flows = append(c.Flows, flow)
	}
	return c, nil
}
