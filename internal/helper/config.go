package helper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/agyard/agsudo/internal/api"
	"github.com/agyard/agsudo/internal/store"
)

const ConfigPath = "/etc/agsudo/config.json"
const SecurePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

type Config struct {
	AllowedUIDs       []int  `json:"allowed_uids"`
	ReviewerUIDs      []int  `json:"reviewer_uids"`
	StateDir          string `json:"state_dir"`
	SecurePath        string `json:"secure_path"`
	DefaultTTLSeconds int64  `json:"default_ttl_seconds"`
	MaxTTLSeconds     int64  `json:"max_ttl_seconds"`
	Dev               bool   `json:"-"`
}

func defaults() Config {
	return Config{StateDir: "/var/lib/agsudo", SecurePath: SecurePath, DefaultTTLSeconds: 600, MaxTTLSeconds: 86400}
}
func (c Config) access(uid int) (store.Access, error) {
	a := store.Access{UID: uid, Reviewer: uid == 0 || slices.Contains(c.ReviewerUIDs, uid)}
	if uid != 0 && !a.Reviewer && !slices.Contains(c.AllowedUIDs, uid) {
		return a, api.Err("forbidden", "UID %d is not configured to use AG-sudo", uid)
	}
	return a, nil
}
func (c Config) canSubmit(uid int) bool { return uid == 0 || slices.Contains(c.AllowedUIDs, uid) }

// Every ancestor must be protected, including paths named in the configuration.
// Checking only the final file would permit replacing it through its parent.
func securePath(path string, uid uint32) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("path must be absolute and clean: %s", path)
	}
	for p := path; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok || sys.Uid != uid || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("untrusted owner, permissions or symlink: %s", p)
		}
		if p == "/" {
			break
		}
	}
	return nil
}

// decodeOne reads exactly one JSON value of at most limit bytes, rejecting
// unknown fields and trailing data.
func decodeOne(r io.Reader, limit int, v any) error {
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err == nil && len(b) > limit {
		err = fmt.Errorf("larger than %d bytes", limit)
	}
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err == nil && d.Decode(new(any)) != io.EOF {
		err = errors.New("trailing data")
	}
	return err
}
func loadProduction() (Config, error) {
	c := defaults()
	if os.Geteuid() != 0 {
		return c, api.Err("not_installed", "agsudo-helper needs a root-owned setuid installation; see README or use make demo for unprivileged development")
	}
	if err := securePath(ConfigPath, 0); err != nil {
		return c, api.Err("configuration", "%s: %v", ConfigPath, err)
	}
	f, err := os.Open(ConfigPath)
	if err != nil {
		return c, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = errors.New("not a regular file")
	}
	if err == nil {
		err = decodeOne(f, 64<<10, &c)
	}
	if err != nil {
		return c, api.Err("configuration", "%s: %v", ConfigPath, err)
	}
	if c.DefaultTTLSeconds < 1 || c.MaxTTLSeconds < c.DefaultTTLSeconds || c.MaxTTLSeconds > 86400 {
		return c, api.Err("configuration", "TTL must satisfy 1 <= default <= max <= 86400 seconds")
	}
	if c.SecurePath == "" {
		return c, api.Err("configuration", "secure_path must not be empty")
	}
	paths := strings.Split(c.SecurePath, ":")
	for i, p := range paths {
		if !filepath.IsAbs(p) {
			return c, api.Err("configuration", "secure_path entries must be absolute")
		}
		// Merged-/usr systems normally use /bin -> /usr/bin and /sbin ->
		// /usr/sbin. Validate the resolved directories, allowing those links.
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			return c, api.Err("configuration", "secure_path entry %s: %v", p, err)
		}
		if err := securePath(resolved, 0); err != nil {
			return c, api.Err("configuration", "secure_path entry %s is not a protected root path: %v", p, err)
		}
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() {
			return c, api.Err("configuration", "secure_path entry %s must be an existing directory", p)
		}
		paths[i] = resolved
	}
	c.SecurePath = strings.Join(paths, ":")
	if err = securePath(c.StateDir, 0); err != nil {
		return c, api.Err("configuration", "state directory: %v (create it with mode 0700)", err)
	}
	st, err = os.Stat(c.StateDir)
	if err != nil {
		return c, err
	}
	if !st.IsDir() || st.Mode().Perm() != 0700 {
		return c, api.Err("configuration", "state directory must be a directory with mode 0700")
	}
	return c, nil
}
