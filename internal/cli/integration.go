package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/agyard/agsudo/internal/api"
	"github.com/spf13/cobra"
)

func newIntegrationCommand(o *options) *cobra.Command {
	root := &cobra.Command{Use: "integration", Short: "Install or uninstall agent host hooks", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	for _, action := range []string{"install", "uninstall"} {
		command := &cobra.Command{Use: action + " <codex|claude>", Short: strings.ToUpper(action[:1]) + action[1:] + " user-level hooks, preserving other settings",
			Args: cobra.ExactArgs(1), ValidArgs: []string{"codex", "claude"},
			RunE: func(cmd *cobra.Command, args []string) error {
				path, err := integrationPath(args[0])
				if err != nil {
					return err
				}
				exe, err := os.Executable()
				if err != nil {
					return err
				}
				exe, err = filepath.EvalSymlinks(exe)
				if err != nil {
					return err
				}
				changed, err := updateIntegration(path, args[0], exe, action == "install")
				if err != nil {
					return err
				}
				message := map[bool]string{true: "Installed", false: "Already installed"}[changed] + " in " + path +
					"\nRestart the host to load the hooks, then check routing with: sudo --version"
				if args[0] == "codex" {
					message += "\nTrust the hook in Codex /hooks. It routes sudo only in Full Access mode."
				}
				if action == "uninstall" {
					message = map[bool]string{true: "Uninstalled from ", false: "Not installed in "}[changed] + path
				}
				return o.print(cmd, map[string]any{"host": args[0], "path": path, "action": action, "changed": changed}, message)
			}}
		root.AddCommand(command)
	}
	root.AddCommand(&cobra.Command{Use: "hook <codex|claude>", Hidden: true, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIntegrationHook(args[0], cmd.InOrStdin(), cmd.OutOrStdout())
		}})
	return root
}

func integrationPath(host string) (string, error) {
	var dir, file string
	switch host {
	case "codex":
		dir, file = os.Getenv("CODEX_HOME"), "hooks.json"
	case "claude":
		dir, file = os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"
	default:
		return "", api.Err("usage", "supported integrations: codex, claude")
	}
	if dir == "" {
		userDir, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(userDir, "."+host)
	}
	if !filepath.IsAbs(dir) {
		return "", api.Err("configuration", "%s config directory must be absolute", host)
	}
	return filepath.Join(dir, file), nil
}

func integrationEvents(host string) []string {
	if host == "codex" {
		return []string{"PreToolUse"}
	}
	return []string{"SessionStart", "CwdChanged"}
}

// A marker inside the supported command field keeps uninstall independent of
// the binary's installation path, without introducing host-specific JSON keys.
func integrationMarker(host string) string { return " # agsudo:" + host }
func shellQuote(s string) string           { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func editIntegration(root map[string]any, host, exe string, install bool) error {
	raw, exists := root["hooks"]
	if !exists {
		if !install {
			return nil
		}
		raw = map[string]any{}
		root["hooks"] = raw
	}
	hooks, ok := raw.(map[string]any)
	if !ok {
		return api.Err("configuration", "hooks must be a JSON object")
	}
	// An identical handler stays where it is; stale ones are removed. Keys are
	// only deleted when emptied by this edit, so a no-op leaves the file alone.
	want := shellQuote(exe) + " integration hook " + host + integrationMarker(host)
	removedAny := false
	for _, event := range integrationEvents(host) {
		groups := []any{}
		if raw, exists := hooks[event]; exists {
			var ok bool
			groups, ok = raw.([]any)
			if !ok {
				return api.Err("configuration", "hooks.%s must be an array", event)
			}
		}
		kept := make([]any, 0, len(groups)+1)
		found, removed := false, false
		for _, raw := range groups {
			group, ok := raw.(map[string]any)
			if !ok {
				return api.Err("configuration", "hooks.%s contains a non-object group", event)
			}
			handlers, ok := group["hooks"].([]any)
			if !ok {
				return api.Err("configuration", "hooks.%s group must contain a hooks array", event)
			}
			remaining := make([]any, 0, len(handlers))
			for _, raw := range handlers {
				handler, ok := raw.(map[string]any)
				if !ok {
					return api.Err("configuration", "hooks.%s contains a non-object handler", event)
				}
				command, _ := handler["command"].(string)
				if handler["type"] == "command" && strings.HasSuffix(command, integrationMarker(host)) {
					if !install || found || command != want {
						removed = true
						continue
					}
					found = true
				}
				remaining = append(remaining, raw)
			}
			if len(remaining) == len(handlers) {
				kept = append(kept, group)
			} else if len(remaining) > 0 {
				group["hooks"] = remaining
				kept = append(kept, group)
			}
		}
		if install && !found {
			group := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": want, "timeout": 10}}}
			if host == "codex" {
				group["matcher"] = "^Bash$"
			}
			kept = append(kept, group)
		}
		if len(kept) > 0 {
			hooks[event] = kept
		} else if removed {
			delete(hooks, event)
		}
		removedAny = removedAny || removed
	}
	if len(hooks) == 0 && removedAny {
		delete(root, "hooks")
	}
	return nil
}

func readIntegrationConfig(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if errors.Is(err, syscall.ELOOP) {
		return nil, api.Err("configuration", "refusing to edit a symlinked config; edit its target instead: %s", path)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) || st.Mode().Perm()&0022 != 0 {
		return nil, api.Err("configuration", "config must be a user-owned regular file, not writable by other users: %s", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 2<<20+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 2<<20 {
		return nil, api.Err("configuration", "configuration exceeds 2 MiB: %s", path)
	}
	return b, nil
}

// updateIntegration merges into the host config and atomically replaces it,
// reporting whether anything changed.
func updateIntegration(path, host, exe string, install bool) (bool, error) {
	if !install {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return false, err
	}
	if !st.IsDir() || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) || st.Mode().Perm()&0022 != 0 {
		return false, api.Err("configuration", "config directory must be user-owned and not writable by other users: %s", dir)
	}
	original, err := readIntegrationConfig(path)
	if err != nil {
		return false, err
	}
	root := map[string]any{}
	if original != nil {
		dec := json.NewDecoder(bytes.NewReader(original))
		dec.UseNumber()
		if err = dec.Decode(&root); err != nil {
			return false, api.Err("configuration", "invalid JSON in %s: %v", path, err)
		}
		if root == nil || dec.Decode(new(any)) != io.EOF {
			return false, api.Err("configuration", "expected one JSON object in %s", path)
		}
	}
	before, err := json.Marshal(root)
	if err != nil {
		return false, err
	}
	if err = editIntegration(root, host, exe, install); err != nil {
		return false, err
	}
	after, err := json.Marshal(root)
	if err != nil || bytes.Equal(before, after) {
		return false, err
	}
	tmp, err := os.CreateTemp(dir, ".agsudo-config-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	// Keep shell commands such as "a && b" readable in the user's config.
	enc := json.NewEncoder(tmp)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err = enc.Encode(root); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	// A host or editor may write concurrently. Refuse an observed concurrent
	// edit rather than replacing it with the earlier version we merged.
	latest, err := readIntegrationConfig(path)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(latest, original) {
		return false, api.Err("conflict", "configuration changed during installation; retry")
	}
	return true, os.Rename(tmp.Name(), path)
}
