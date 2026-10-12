package cli

import (
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/thezzisu/agsudo/internal/api"
)

const codexHookMarker = "# agsudo environment v1\n"

var hostSessionPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

type hookEvent struct {
	Event          string                     `json:"hook_event_name"`
	Tool           string                     `json:"tool_name"`
	Session        string                     `json:"session_id"`
	PermissionMode string                     `json:"permission_mode"`
	Input          map[string]json.RawMessage `json:"tool_input"`
}

func sessionLabel(host, id string) string {
	if hostSessionPattern.MatchString(id) {
		return id
	}
	return host + ":" + api.Hash([]byte(id))[:32]
}

func integrationEnvironment(dir, host, session string) string {
	// Reapply the shim at the front even if a host environment manager moved it.
	return "case \"$PATH\" in " + shellQuote(dir) + "|" + shellQuote(dir) + ":*) ;; *) export PATH=" + shellQuote(dir) + ":\"$PATH\" ;; esac\nexport AGSUDO_SESSION=" + shellQuote(sessionLabel(host, session)) + "\n"
}

func runIntegrationHook(host string, input io.Reader, output io.Writer) error {
	if host != "codex" && host != "claude" {
		return api.Err("usage", "supported integrations: codex, claude")
	}
	b, err := io.ReadAll(io.LimitReader(input, api.MaxMessage+1))
	if err != nil {
		return err
	}
	if len(b) > api.MaxMessage {
		return api.Err("invalid_input", "hook input exceeds 1 MiB")
	}
	var event hookEvent
	if err = json.Unmarshal(b, &event); err != nil {
		return api.Err("invalid_input", "invalid hook JSON: %v", err)
	}
	if host == "claude" {
		if event.Event != "SessionStart" && event.Event != "CwdChanged" {
			return nil
		}
		path := os.Getenv("CLAUDE_ENV_FILE")
		if path == "" {
			return api.Err("configuration", "Claude did not provide CLAUDE_ENV_FILE")
		}
		dir, err := ensureShim()
		if err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.WriteString(f, integrationEnvironment(dir, host, event.Session))
		return err
	}
	if event.Event != "PreToolUse" || event.Tool != "Bash" {
		return nil
	}
	var command string
	if err = json.Unmarshal(event.Input["command"], &command); err != nil {
		return api.Err("invalid_input", "Bash hook requires tool_input.command")
	}
	if strings.HasPrefix(command, codexHookMarker) {
		return nil
	}
	// Codex requires allow along with updatedInput. Do not silently change an
	// interactive/plan-mode session's tool approval policy to inject PATH.
	if event.PermissionMode != "bypassPermissions" {
		return codexHookResult(output, "deny", "AG-sudo's Codex hook requires Full Access. Uninstall it for other permission modes.", nil)
	}
	dir, err := ensureShim()
	if err != nil {
		return codexHookResult(output, "deny", "AG-sudo could not prepare sudo: "+err.Error(), nil)
	}
	preamble := codexHookMarker + integrationEnvironment(dir, host, event.Session)
	// Preserve every other tool argument. The original shell parses the command
	// after this preamble; scripts, pipelines and heredocs keep their semantics.
	updated := make(map[string]json.RawMessage, len(event.Input))
	for key, value := range event.Input {
		updated[key] = value
	}
	updated["command"], err = json.Marshal(preamble + command)
	if err != nil {
		return err
	}
	return codexHookResult(output, "allow", "", updated)
}

func codexHookResult(output io.Writer, decision, reason string, updated any) error {
	result := map[string]any{"hookEventName": "PreToolUse", "permissionDecision": decision}
	if reason != "" {
		result["permissionDecisionReason"] = reason
	}
	if updated != nil {
		result["updatedInput"] = updated
	}
	return json.NewEncoder(output).Encode(map[string]any{"hookSpecificOutput": result})
}
