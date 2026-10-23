package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emjin/agent-engine/internal/minimaxacp"
)

func TestStockProcessPolicy_DeniesNewAliasesAndUnapprovedHelper(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS candidate")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("SENTINEL"), 0600); err != nil {
		t.Fatal(err)
	}
	var helperCalls atomic.Int32
	helper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		helperCalls.Add(1)
		if err := os.WriteFile(outside, []byte("HELPER_WRITE"), 0600); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, "HELPER_DONE")
	}))
	defer helper.Close()
	// Verify the helper really writes when reached without the boundary.
	control, err := http.Get(helper.URL)
	if err != nil {
		t.Fatal(err)
	}
	control.Body.Close()
	content, err := os.ReadFile(outside)
	if err != nil || string(content) != "HELPER_WRITE" || helperCalls.Load() != 1 {
		t.Fatal("helper control failed", err)
	}
	helperCalls.Store(0)
	if err := os.WriteFile(outside, []byte("SENTINEL"), 0600); err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "PROVIDER_OK") }))
	defer provider.Close()
	providerURL, err := url.Parse(provider.URL)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(workspace, "config")
	publication := filepath.Join(workspace, "v2", "migration", "manifests")
	if err := os.MkdirAll(publication, 0700); err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(publication, "publish-temp")
	if err := os.WriteFile(temporary, []byte("METADATA"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("FROZEN_CONFIG"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := stockProcessPolicy("(subpath "+strconv.Quote(workspace)+")", filepath.Join(workspace, "protected"), config, providerURL.Port())
	if output, err := exec.Command("/usr/bin/sandbox-exec", "-p", policy, "/bin/ln", temporary, filepath.Join(publication, "published")).CombinedOutput(); err != nil {
		t.Fatalf("stock metadata publication control failed: %v %s", err, output)
	}
	if output, err := exec.Command("/usr/bin/sandbox-exec", "-p", policy, "/bin/ln", config, filepath.Join(publication, "config-alias")).CombinedOutput(); err == nil || !explicitDenial(string(output)) {
		t.Fatalf("frozen config could be aliased: %v %s", err, output)
	}
	if output, err := exec.Command("/usr/bin/sandbox-exec", "-p", policy, "/bin/ln", outside, filepath.Join(workspace, "alias")).CombinedOutput(); err == nil || !explicitDenial(string(output)) {
		t.Fatalf("hard-link creation not denied: %v %s", err, output)
	}
	curl := func(address string) ([]byte, error) {
		return exec.Command("/usr/bin/sandbox-exec", "-p", policy, "/usr/bin/curl", "--noproxy", "*", "--fail", "--silent", "--show-error", "--max-time", "3", address).CombinedOutput()
	}
	if output, err := curl(provider.URL); err != nil || string(output) != "PROVIDER_OK" {
		t.Fatalf("provider control failed: %v %s", err, output)
	}
	if output, err := curl(helper.URL); err == nil {
		t.Fatalf("unapproved helper reached: %s", output)
	}
	content, err = os.ReadFile(outside)
	if err != nil || string(content) != "SENTINEL" || helperCalls.Load() != 0 {
		t.Fatal("helper bypassed boundary", err)
	}
}

func TestAssessProbe_RequiresExactEffectsAndProtectsOtherSentinels(t *testing.T) {
	hash := func(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
	for _, tc := range []struct {
		name                    string
		before, after, expected map[string]string
		allowed, want           bool
	}{
		{"edit", map[string]string{"src": hash("OLD"), "secret": hash("SECRET")}, map[string]string{"src": hash("NEW"), "secret": hash("SECRET")}, map[string]string{"src": "NEW"}, true, true},
		{"wrong-content", map[string]string{"src": hash("OLD")}, map[string]string{"src": hash("OTHER")}, map[string]string{"src": "NEW"}, true, false},
		{"also-wrote-secret", map[string]string{"src": hash("OLD"), "secret": hash("SECRET")}, map[string]string{"src": hash("NEW"), "secret": hash("NEW")}, map[string]string{"src": "NEW"}, true, false},
		{"delete", map[string]string{"src": hash("OLD")}, map[string]string{"src": "MISSING"}, map[string]string{"src": "MISSING"}, true, true},
		{"rename", map[string]string{"src": hash("OLD"), "dest": "MISSING"}, map[string]string{"src": "MISSING", "dest": hash("OLD")}, map[string]string{"src": "MISSING", "dest": "OLD"}, true, true},
		{"unmeasured-effect", map[string]string{"src": hash("OLD")}, map[string]string{"src": hash("NEW")}, map[string]string{"src": "NEW", "unknown": "NEW"}, true, false},
		{"already-deleted", map[string]string{"src": "MISSING"}, map[string]string{"src": "MISSING"}, map[string]string{"src": "MISSING"}, true, false},
		{"denied", map[string]string{"secret": hash("SECRET")}, map[string]string{"secret": hash("SECRET")}, nil, false, true},
		{"denied-but-changed", map[string]string{"secret": hash("SECRET")}, map[string]string{"secret": hash("NEW")}, nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &probe{Before: tc.before, After: tc.after, Expected: tc.expected, Allowed: tc.allowed}
			if got := assessProbe(p, "real-tool", "Operation not permitted"); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	p := &probe{Before: map[string]string{"secret": hash("SECRET")}, After: map[string]string{"secret": hash("SECRET")}}
	if assessProbe(p, "", "Operation not permitted") || assessProbe(p, "real-tool", "invalid arguments") {
		t.Fatal("unexecuted/invalid action credited as a denial")
	}
}

func TestDenial_DoesNotCreditAnUnexecutedOrInvalidTool(t *testing.T) {
	for _, result := range []string{"", "invalid arguments", "file must be read before editing", "Successfully wrote file"} {
		if explicitDenial(result) {
			t.Fatalf("non-enforcement credited: %s", result)
		}
	}
	if !explicitDenial("Operation not permitted") {
		t.Fatal("explicit OS denial missed")
	}
}

func TestStockProcessPolicy_NullDeviceDoesNotPermitProtectedWrites(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS candidate policy")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(root, "protected")
	config := filepath.Join(root, "config")
	for _, path := range []string{protected, config} {
		if err := os.WriteFile(path, []byte("SENTINEL"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	policy := stockProcessPolicy("(subpath "+strconv.Quote(root)+")", protected, config)
	if output, err := exec.Command("/usr/bin/sandbox-exec", "-p", policy, "/bin/sh", "-c", "printf GUARDIAN_READY >/dev/null").CombinedOutput(); err != nil {
		t.Fatalf("null device control failed: %v: %s", err, output)
	}
	for _, path := range []string{protected, config} {
		output, err := exec.Command("/usr/bin/sandbox-exec", "-p", policy, "/bin/sh", "-c", "printf FORBIDDEN > \"$1\"", "boundary-test", path).CombinedOutput()
		if err == nil || !explicitDenial(string(output)) {
			t.Fatalf("protected write not denied: %v: %s", err, output)
		}
		content, err := os.ReadFile(path)
		if err != nil || string(content) != "SENTINEL" {
			t.Fatalf("protected sentinel changed: %v: %q", err, content)
		}
	}
}

func TestFrozenPermission_RequiresAdvertisedExactReadback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session *minimaxacp.Session
		valid   bool
	}{
		{"missing-session", nil, false},
		{"missing-option", &minimaxacp.Session{}, false},
		{"wrong-readback", &minimaxacp.Session{Options: []minimaxacp.ConfigOption{{ID: "permissionMode", CurrentValue: "default", Options: []string{"bypassPermissions"}}}}, false},
		{"unadvertised", &minimaxacp.Session{Options: []minimaxacp.ConfigOption{{ID: "permissionMode", CurrentValue: "bypassPermissions"}}}, false},
		{"verified", &minimaxacp.Session{Options: []minimaxacp.ConfigOption{{ID: "permissionMode", CurrentValue: "bypassPermissions", Options: []string{"bypassPermissions"}}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := requireFrozenPermission(tc.session); (got == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, got)
			}
		})
	}
}

func TestProviderPreflightDoesNotConsumeFilesystemProbe(t *testing.T) {
	p := &probe{Name: "allowed-write", ToolKind: "write", Target: "/disposable/source"}
	f := &fixtureProvider{current: p}
	tools := `"tools":[{"name":"write","input_schema":{"properties":{"file_path":{},"content":{}}}}]`
	preflight := httptest.NewRecorder()
	f.ServeHTTP(preflight, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"stream":false,`+tools+`}`)))
	if p.Called != "" || strings.Contains(preflight.Body.String(), "tool_use") {
		t.Fatal("availability check consumed the real probe")
	}
	turn := httptest.NewRecorder()
	f.ServeHTTP(turn, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"stream":true,`+tools+`}`)))
	if p.Called != "write" || !strings.Contains(turn.Body.String(), "input_json_delta") {
		t.Fatal("actual turn did not request the real tool")
	}
}

func TestProviderOnlyCollectsCurrentToolResult(t *testing.T) {
	p := &probe{Name: "current", Called: "write"}
	f := &fixtureProvider{current: p}
	call := func(id string) {
		f.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"stream":true,"messages":[{"content":[{"type":"tool_result","tool_use_id":"`+id+`","content":"denied"}]}]}`)))
	}
	call("boundary-tool-previous")
	if p.Result != "" {
		t.Fatal("previous probe's result reused")
	}
	call("boundary-tool-current")
	if p.Result != `"denied"` {
		t.Fatal("correlated tool result missing")
	}
}
