package control

import "strings"

// MiniMax account region and credential-source readiness.
//
// The operator authenticates MiniMax once, through the installed `mcode`
// client, and every ACP child this layer launches reuses THAT login. This file
// exists to model the two operator settings that login depends on — which
// account region was logged in, and which credential source the provider is
// configured to read — and to refuse when they are missing or unrecognised.
//
// What this file deliberately does NOT do is anything with the credential
// itself. It does not read an auth-state file, does not copy one, does not parse
// a token or API key, and does not hash one for an artifact or a log. There is
// no code path here that can emit a secret because there is no field capable of
// holding one: the entire credential-facing surface is the closed enums below
// and the booleans reported by `mcode provider list --json`, which reports
// whether a provider has a key as `hasApiKey: true|false` and never as a value.
//
// The reason for that restraint is a failure mode rather than a style
// preference. A credential this layer copied would become a credential this
// layer stores, transports to a child process, and can leak through an artifact
// or a diagnostic dump — and a leaked key is an incident the operator cannot
// undo by editing a file. The operator's own `mcode login` is the one authority
// for the credential, and it already knows where the operator keeps it.
//
// Region is modelled explicitly rather than defaulted because the two MiniMax
// deployments are separate accounts with separate logins. A profile that
// silently assumed "cn" and was really aimed at a Global account produces an
// authentication failure at the first turn, which reads like a broken
// configuration, a wrong model selector, or a transport bug. Naming the region
// at parse time moves that diagnosis to a refusal that can be read.

// AccountRegion is which MiniMax deployment an operator's login belongs to.
//
// It is a closed set on purpose. `mcode login --help` accepts exactly `cn` and
// `global`; anything else is a typo or an account this code was never verified
// against, and treating it as one of the two would route work to a deployment
// the operator did not choose.
type AccountRegion string

const (
	// RegionCN is the China account, authenticated by `mcode login`.
	RegionCN AccountRegion = "cn"
	// RegionGlobal is the Global account, authenticated by
	// `mcode login --region global`.
	RegionGlobal AccountRegion = "global"
)

// RegionCredentialSource is which supported credential source the installed
// client is configured to read.
//
// It is recorded because `mcode provider use token-plan|api-key` is the
// supported way to choose it, and because "the operator believes they are on the
// token plan" is not something this layer can verify without reading the
// credential. What it can check is whether the client reports an active,
// enabled provider that has a key — all booleans, none of them the key.
type RegionCredentialSource string

const (
	// CredentialSourceTokenPlan is the token plan selected through
	// `mcode provider use token-plan`.
	CredentialSourceTokenPlan RegionCredentialSource = "token-plan"
	// CredentialSourceAPIKey is an API key selected through
	// `mcode provider use api-key`. The key itself is the operator's business
	// and never reaches this package.
	CredentialSourceAPIKey RegionCredentialSource = "api-key"
)

// operatorRegionLogin is the command that authenticates each region.
//
// These are the exact strings an operator runs themselves. They are reported in
// refusals precisely so this layer never has to run them: a browser login is a
// human step, and a control plane that authenticated on the operator's behalf
// would be a control plane holding a credential.
const (
	operatorRegionLoginCN     = "mcode login"
	operatorRegionLoginGlobal = "mcode login --region global"
)

// operatorProviderUse is how the credential source is selected.
const operatorProviderUse = "mcode provider use token-plan|api-key"

// LoginCommand is the operator command that authenticates this region.
//
// It is a rendering of a refused operator setting, not an action this package
// takes. Nothing here runs it.
func (r AccountRegion) LoginCommand() string {
	switch r {
	case RegionCN:
		return operatorRegionLoginCN
	case RegionGlobal:
		return operatorRegionLoginGlobal
	default:
		return "mcode login  (or: mcode login --region global)"
	}
}

// String renders the region for operator-facing text.
func (r AccountRegion) String() string { return string(r) }

// parseAccountRegion resolves an operator-written region at parse time.
//
// The refusal names the setting and the command, because "unrecognised region"
// on its own would send the operator looking through the ACP transport instead
// of at their own configuration line. An unrecognised value is never coerced to
// a default: the failure of a wrong region is an authentication error the
// operator would misdiagnose as a transport fault.
func parseAccountRegion(field, raw string) (AccountRegion, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	switch AccountRegion(normalized) {
	case RegionCN:
		return RegionCN, nil
	case RegionGlobal:
		return RegionGlobal, nil
	case "":
		return "", cfgErr(field, "required for a %s profile; it selects which MiniMax deployment the operator's own login authenticates, and assuming one would surface as an authentication failure at the first turn that looks like a transport fault. Set it to \"cn\" or \"global\", then authenticate with the matching command: %s",
			TransportMiniMaxACP, operatorRegionLoginCN+"  /  "+operatorRegionLoginGlobal)
	default:
		return "", cfgErr(field, "must be one of cn, global; got %q. An unrecognised region is refused rather than coerced to a default, because a request routed at the wrong deployment fails as an authentication error that reads like a transport fault. Authenticate the account you meant: %s",
			raw, operatorRegionLoginCN+"  /  "+operatorRegionLoginGlobal)
	}
}

// ProviderObservation is the credential-free projection of
// `mcode provider list --json` for one provider.
//
// Every field is a boolean or a closed enum, and that is the whole design: the
// installed client already answers "is this provider usable" without disclosing
// anything, so this layer never needs to open an auth-state file to learn the
// same thing. There is deliberately no field that could hold a token, an API key
// or a file path, so no observation built here can become a leak by accident.
type ProviderObservation struct {
	// ProviderID is the provider's name, as the client reports it.
	ProviderID string
	// Active is the provider the client will actually use.
	Active bool
	// Enabled is whether the provider is turned on at all.
	Enabled bool
	// HasAPIKey is the client's boolean statement that a credential is
	// configured. It is never the credential.
	HasAPIKey bool
	// Source is which supported credential source the client is configured to
	// read.
	Source RegionCredentialSource
}

// CredentialReadiness is whether an operator login can serve an ACP profile.
//
// It is an enum rather than a bool so that "nobody has looked yet" cannot be
// confused with "looked, and it is fine". Collapsing those two is how an
// unverified credential source ends up reported as ready.
type CredentialReadiness string

const (
	// CredentialReadinessUnknown means no observation was supplied, so nothing
	// has been established. It is deliberately not "ready".
	CredentialReadinessUnknown CredentialReadiness = "unknown"
	// CredentialReadinessReady means the observed provider is active, enabled,
	// has a credential, and names a supported source.
	CredentialReadinessReady CredentialReadiness = "ready"
	// CredentialReadinessNotReady means the observed provider cannot serve a
	// request: it is inactive, disabled, has no credential, or names a source
	// this build does not recognise.
	CredentialReadinessNotReady CredentialReadiness = "not-ready"
)

// Ready reports whether readiness is established and positive.
func (r CredentialReadiness) Ready() bool { return r == CredentialReadinessReady }

// CredentialReadiness derives readiness from a credential-free observation.
//
// The inputs are exactly what the operator's own client reports, and the output
// is an enum. Nothing in this function reads, derives from, or approximates a
// credential, and nothing it returns could leak one: an unusable login and a
// wrong-region login are the same non-ready answer here, which is honest,
// because this layer cannot tell those two apart without a live-turn
// observation it is not permitted to make.
func CredentialReadinessFor(obs ProviderObservation) CredentialReadiness {
	switch obs.Source {
	case CredentialSourceTokenPlan, CredentialSourceAPIKey:
	default:
		// No source, or a source outside the supported set. `mcode provider use`
		// is the supported way to choose one, and an unsupported value is not
		// something to guess at.
		return CredentialReadinessNotReady
	}
	if !obs.Active || !obs.Enabled || !obs.HasAPIKey {
		return CredentialReadinessNotReady
	}
	return CredentialReadinessReady
}

// Readiness computes this profile's credential readiness from an observation.
//
// It refuses to report anything for a profile whose region is unrecorded,
// because readiness without a region cannot name the login command that would
// fix it, and a refusal that omits the fix is a refusal that gets retried.
func (p Profile) Readiness(obs ProviderObservation) CredentialReadiness {
	if p.AccountRegion == "" {
		return CredentialReadinessUnknown
	}
	return CredentialReadinessFor(obs)
}

// ReadinessRefusal explains why a profile cannot serve a request yet.
//
// It names the missing operator setting and the exact command, because the two
// things an operator needs in order to unblock this are which line of
// CONTROL.toml to add and what to run in their own terminal. It reports no
// credential-derived detail: the observation's booleans are the most this layer
// is allowed to say about authentication.
func (p Profile) ReadinessRefusal(obs ProviderObservation) error {
	if p.AccountRegion == "" {
		return cfgErr("profiles."+p.Name+".region", "%s",
			"required before "+string(TransportMiniMaxACP)+" can use the operator's login; set it to \"cn\" or \"global\" in CONTROL.toml, then run "+p.regionLoginHint())
	}
	if got := CredentialReadinessFor(obs); !got.Ready() {
		return cfgErr("profiles."+p.Name, "%s", "the operator's "+string(TransportMiniMaxACP)+
			" credential source is not usable as observed ("+string(got)+") for region "+p.AccountRegion.String()+
			": the client reports the provider must be active, enabled, have a credential, and be set to a supported source with `"+
			operatorProviderUse+"`. Authenticate the "+p.AccountRegion.String()+" account yourself with `"+
			p.AccountRegion.LoginCommand()+"`, then select the source with `"+operatorProviderUse+
			"`. Agent Engine reuses that login and never reads, copies or stores the credential itself")
	}
	return nil
}

// regionLoginHint is the operator command for a profile whose region is
// unrecorded. It names both, because the missing setting IS the choice between
// them.
func (p Profile) regionLoginHint() string {
	return operatorRegionLoginCN + "  /  " + operatorRegionLoginGlobal
}

// requiresAccountRegion reports whether this profile's transport reuses an
// operator login that has to be attributed to a region.
//
// Only the native ACP transport qualifies. Agent Deck sessions reach their own
// already-authenticated provider CLIs, which this layer does not launch or
// reason about, so requiring a MiniMax region there would be a field about a
// different tool.
func (p Profile) requiresAccountRegion() bool {
	return p.TransportFor() == TransportMiniMaxACP
}
