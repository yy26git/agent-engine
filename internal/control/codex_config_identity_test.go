package control

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexConfigFingerprint_StartupCountersAndFormatting(t *testing.T) {
	base := "model = 'selected'\n[tui]\nscreen_reader_detection_done = true\n"
	want, err := codexConfigFingerprint([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	for _, config := range []string{
		base + "[tui.model_availability_nux]\nselected = 1\n",
		base + "[tui.model_availability_nux]\nselected = 2\nother = 100\n",
		"# presentation changed\nmodel = \"selected\"\ntui = {model_availability_nux = {selected = 3}, screen_reader_detection_done = true}\n",
	} {
		got, err := codexConfigFingerprint([]byte(config))
		if err != nil || got != want {
			t.Fatalf("equivalent configuration changed identity: %s %v", got, err)
		}
	}
	for _, config := range []string{"", "[tui.model_availability_nux]\nselected=1\n"} {
		got, err := codexConfigFingerprint([]byte(config))
		if err != nil {
			t.Fatal(err)
		}
		empty, _ := codexConfigFingerprint(nil)
		if got != empty {
			t.Fatal("startup-only table changed empty configuration")
		}
	}
}

func TestCodexConfigFingerprint_PinsSecurityAndUnknownValues(t *testing.T) {
	for _, pair := range [][2]string{
		{"model='a'", "model='b'"},
		{"sandbox_mode='read-only'", "sandbox_mode='workspace-write'"},
		{"approval_policy='never'", "approval_policy='on-request'"},
		{"[projects.'/source']\ntrust_level='trusted'", "[projects.'/source']\ntrust_level='untrusted'"},
		{"[tui]\nnotifications=false", "[tui]\nnotifications=['agent-turn-complete']"},
		{"[tui]\nscreen_reader_detection_done=true", "[tui]\nscreen_reader_detection_done=false"},
		{"[tui]\nfuture_security_flag=false", "[tui]\nfuture_security_flag=true"},
		{"unknown=1", "unknown=1.0"},
		{"unknown='1979-05-27T07:32:00Z'", "unknown=1979-05-27T07:32:00Z"},
		{"[tui]\n\"model_availability_nux.selected\"=1", "[tui]\n\"model_availability_nux.selected\"=2"},
	} {
		a, err := codexConfigFingerprint([]byte(pair[0]))
		if err != nil {
			t.Fatal(err)
		}
		b, err := codexConfigFingerprint([]byte(pair[1]))
		if err != nil || a == b {
			t.Fatalf("meaningful change ignored: %q -> %q (%v)", pair[0], pair[1], err)
		}
	}
}

func TestCodexConfigFingerprint_RejectsMalformedAndExternalConfiguration(t *testing.T) {
	for _, config := range []string{
		"model='a'\nmodel='b'", "[tui", "tui='not-a-table'",
		"[tui]\nmodel_availability_nux='not-a-table'",
		"[tui.model_availability_nux]\nselected=-1",
		"[tui.model_availability_nux]\nselected='1'",
		"[tui.model_availability_nux]\nselected=1.0",
		"[tui.model_availability_nux]\nselected={value=1}",
		"[tui.model_availability_nux]\nselected={mcp_servers={writer={command='writer'}}}",
		"[\"mcp_\\u0073ervers\".writer]\ncommand='writer'",
		"mcp_servers={writer={command='writer#hidden'}}",
		"notify=['writer']", "[hooks]\ncommand='writer'",
		"[[agents]]\nmcp_servers={writer={command='writer'}}",
		"agents=[{mcp_servers={writer={command='writer'}}}]",
		"unknown=[[{hooks={command='writer'}}]]",
	} {
		if _, err := codexConfigFingerprint([]byte(config)); err == nil {
			t.Fatalf("unsafe/malformed configuration qualified: %q", config)
		}
	}
}

func TestConfinementIdentity_CountersStayStableButSecurityChangesInvalidate(t *testing.T) {
	c, p, workspace := qualifiedFixture(t)
	path := filepath.Join(p.LaunchEnv["CODEX_HOME"], "config.toml")
	write := func(config string) string {
		t.Helper()
		if err := os.WriteFile(path, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		id, err := c.ConfinementIdentity(p, workspace)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	one := write("model='selected'\n[tui.model_availability_nux]\nselected=1\n")
	two := write("model='selected'\n[tui.model_availability_nux]\nselected=2\n")
	if one != two {
		t.Fatal("startup invalidated confinement identity")
	}
	changed := write("model='different'\n[tui.model_availability_nux]\nselected=2\n")
	if changed == one {
		t.Fatal("model change inherited old confinement identity")
	}
}
