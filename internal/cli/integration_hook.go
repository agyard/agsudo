package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/agyard/agsudo/internal/api"
)

type hookEvent struct {
	Event          string                     `json:"hook_event_name"`
	Tool           string                     `json:"tool_name"`
	Session        string                     `json:"session_id"`
	PermissionMode string                     `json:"permission_mode"`
	Input          map[string]json.RawMessage `json:"tool_input"`
}

func sessionLabel(host, id string) string {
	if id != "" && api.ValidSession(id) {
		return id
	}
	return host + ":" + api.Hash([]byte(id))[:32]
}

// integrationEnvironment is one shell line, so it barely shifts line numbers
// in the host's errors. The trailing comment also marks rewritten commands.
func integrationEnvironment(dir, host, session string) string {
	return "export PATH=" + shellQuote(dir) + ":\"$PATH\" AGSUDO_SESSION=" + shellQuote(sessionLabel(host, session)) + integrationMarker(host) + "\n"
}

// Claude adds SessionStart output to the model's context.
const sessionNotice = "sudo in this session waits for a person or reviewing agent to approve each command (AG-sudo). If a sudo call times out, its request stays pending: ask for approval of the request ID it printed, then run the same command again. Details: sudo --help"

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
		if _, err = io.WriteString(f, integrationEnvironment(dir, host, event.Session)); err != nil || event.Event != "SessionStart" {
			return err
		}
		_, err = fmt.Fprintln(output, sessionNotice)
		return err
	}
	if event.Event != "PreToolUse" || event.Tool != "Bash" {
		return nil
	}
	var command string
	if err = json.Unmarshal(event.Input["command"], &command); err != nil {
		return api.Err("invalid_input", "Bash hook requires tool_input.command")
	}
	// Codex requires allow along with updatedInput. Outside Full Access, leave
	// the command and the host's approval policy alone; sudo is not routed.
	first, _, _ := strings.Cut(command, "\n")
	if event.PermissionMode != "bypassPermissions" || strings.HasSuffix(first, integrationMarker(host)) {
		return nil
	}
	dir, err := ensureShim()
	if err != nil {
		return codexHookResult(output, "deny", "AG-sudo could not prepare sudo: "+err.Error(), nil)
	}
	preamble := integrationEnvironment(dir, host, event.Session)
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
