// Module agent-engine is Agent Engine, the external engineering control layer.
//
// It is deliberately a separate module from Agent Deck. The control layer owns
// engineering authority — admission, obligations, verification and results —
// while Agent Deck owns execution: sessions, tools, lifecycle, status and the
// event log. Keeping them apart means the control layer can only reach Agent
// Deck through its public CLI and NDJSON output, because it cannot import
// Agent Deck's internal packages at all.
//
// That is a feature, not a limitation. The alternative — an embedded workflow
// package — can reach into the session and event internals, and then nothing
// stops a policy decision from quietly depending on a session detail that a
// later upstream change invalidates.
module github.com/emjin/agent-engine

go 1.24

require github.com/BurntSushi/toml v1.6.0

require (
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2
	golang.org/x/text v0.14.0 // indirect
)
