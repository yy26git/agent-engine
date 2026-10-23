package main

import (
	"io"
	"runtime"
	"runtime/debug"

	"github.com/emjin/agent-engine/internal/control"
)

var buildVersion = "dev"

func binaryIdentity() map[string]any {
	row := map[string]any{"product": "agent-engine", "display_name": "Agent Engine", "version": buildVersion, "go_version": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH}
	if info, ok := debug.ReadBuildInfo(); ok {
		if buildVersion == "dev" && info.Main.Version != "" {
			row["version"] = info.Main.Version
		}
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				row["source_revision"] = s.Value
			}
			if s.Key == "vcs.modified" {
				row["source_modified"] = s.Value == "true"
			}
		}
	}
	return row
}

func capabilities() map[string]any {
	return map[string]any{"schema_version": 1, "product": "agent-engine", "display_name": "Agent Engine", "protocol_versions": []int{1}, "state_schema": control.CurrentSchemaVersion, "identity": binaryIdentity(), "operations": []string{"discover", "episode-discovery", "episode-events", "episode-watch", "exact-digest-plan-acceptance", "imported-implementation-plan", "report-only", "structured-output"}, "automatic_chat_wakeup": false}
}

func cmdIdentity(command string, args []string, stdout, stderr io.Writer, asJSON bool) int {
	fs := flagSet(command, stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return 2
	}
	if command == "version" {
		return emit(stdout, stderr, binaryIdentity())
	}
	return emit(stdout, stderr, capabilities())
}
