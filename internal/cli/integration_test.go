package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrationConfigRoundTrip(t *testing.T) {
	for _, host := range []string{"codex", "claude"} {
		t.Run(host, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			original := []byte(`{"unrelated":9007199254740993,"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"keep-me && true"}]}]}}`)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			for i, exe := range []string{"/first path/agsudo", "/first path/agsudo", "/new'path/agsudo"} {
				changed, err := updateIntegration(path, host, exe, true)
				if err != nil || changed == (i == 1) {
					t.Fatalf("install %d: changed=%v %v", i, changed, err)
				}
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if got := bytes.Count(b, []byte(integrationMarker(host))); got != len(integrationEvents(host)) {
					t.Fatalf("duplicated or missing hooks: %s", b)
				}
				if !bytes.Contains(b, []byte("9007199254740993")) || !bytes.Contains(b, []byte("keep-me && true")) {
					t.Fatalf("lost unrelated settings: %s", b)
				}
			}
			if _, err := updateIntegration(path, host, "/another/agsudo", false); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err = json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(original, &want); err != nil {
				t.Fatal(err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("uninstall changed unrelated settings: %s", b)
			}
		})
	}
}

func TestIntegrationRefusesMalformedConfig(t *testing.T) {
	for _, value := range []string{`null`, `[]`, `{"hooks":[]}`, `{"hooks":{"PreToolUse":{}}}`, `{"hooks":{"PreToolUse":[{"hooks":"wrong"}]}}`, `{`} {
		path := filepath.Join(t.TempDir(), "hooks.json")
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := updateIntegration(path, "codex", "/bin/agsudo", true); err == nil {
			t.Fatalf("accepted %s", value)
		}
		b, err := os.ReadFile(path)
		if err != nil || string(b) != value {
			t.Fatalf("damaged config: %s %v", b, err)
		}
	}
}

func TestIntegrationNoOpsLeaveConfigAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	if err := os.WriteFile(path, []byte(`{"hooks":{"PreToolUse":[]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if changed, err := updateIntegration(path, "codex", "/bin/agsudo", false); changed || err != nil {
		t.Fatalf("uninstall without a handler changed the config: %v", err)
	}
	if _, err := updateIntegration(path, "codex", "/bin/agsudo", true); err != nil {
		t.Fatal(err)
	}
	root := map[string]any{}
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &root)
	hooks := root["hooks"].(map[string]any)
	hooks["PreToolUse"] = append(hooks["PreToolUse"].([]any), map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "later"}}})
	b, _ = json.Marshal(root)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if changed, err := updateIntegration(path, "codex", "/bin/agsudo", true); changed || err != nil {
		t.Fatalf("reinstall moved an identical handler: %v", err)
	}
}

func TestUninstallPreservesSiblingHandlers(t *testing.T) {
	root := map[string]any{"hooks": map[string]any{"PreToolUse": []any{map[string]any{"matcher": "^Bash$", "hooks": []any{
		map[string]any{"type": "command", "command": "'/old/agsudo' integration hook codex" + integrationMarker("codex")},
		map[string]any{"type": "command", "command": "echo 'integration hook codex'"},
	}}}}}
	if err := editIntegration(root, "codex", "/new/agsudo", false); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "/old/") || !strings.Contains(string(b), "echo") {
		t.Fatalf("lost sibling: %s", b)
	}
}

func TestHookOnlyRewritesFullAccessBash(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for _, tc := range []struct{ event, tool, mode, decision string }{
		{"PostToolUse", "Bash", "bypassPermissions", ""},
		{"PreToolUse", "Write", "bypassPermissions", ""},
		{"PreToolUse", "Bash", "plan", ""},
		{"PreToolUse", "Bash", "", ""},
		{"PreToolUse", "Bash", "bypassPermissions", "allow"},
	} {
		input, err := json.Marshal(map[string]any{"hook_event_name": tc.event, "tool_name": tc.tool, "permission_mode": tc.mode, "tool_input": map[string]any{"command": "sudo id"}})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := runIntegrationHook("codex", bytes.NewReader(input), &out); err != nil {
			t.Fatal(err)
		}
		if tc.decision == "" {
			if out.Len() != 0 {
				t.Fatalf("changed unrelated event: %s", out.String())
			}
			continue
		}
		var response struct {
			Output struct {
				Decision string `json:"permissionDecision"`
				Input    struct {
					Command string `json:"command"`
				} `json:"updatedInput"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(out.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Output.Decision != tc.decision || strings.Count(response.Output.Input.Command, "\n") != 1 {
			t.Fatalf("wrong rewrite: %s", out.String())
		}
		// A rewritten command is not rewritten again.
		input, _ = json.Marshal(map[string]any{"hook_event_name": tc.event, "tool_name": tc.tool, "permission_mode": tc.mode, "tool_input": map[string]any{"command": response.Output.Input.Command}})
		out.Reset()
		if err := runIntegrationHook("codex", bytes.NewReader(input), &out); err != nil || out.Len() != 0 {
			t.Fatalf("rewrote twice: %s %v", out.String(), err)
		}
	}
}
