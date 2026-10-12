//go:build dev

package helper

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/thezzisu/agsudo/internal/api"
)

// The development helper can never be used for privilege elevation, even if
// somebody accidentally installs it setuid. Production has no env path override.
func loadConfig() (Config, error) {
	c := defaults()
	c.Dev = true
	if os.Getuid() == 0 || os.Getuid() != os.Geteuid() || os.Getgid() != os.Getegid() {
		return c, api.Err("unsafe_dev_mode", "development helper refuses root or elevated credentials")
	}
	c.StateDir = os.Getenv("AGSUDO_DEV_STATE")
	if c.StateDir == "" || !filepath.IsAbs(c.StateDir) {
		return c, api.Err("configuration", "set AGSUDO_DEV_STATE to an absolute private development directory")
	}
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		return c, err
	}
	st, err := os.Lstat(c.StateDir)
	if err != nil {
		return c, err
	}
	if !st.IsDir() || st.Mode().Perm() != 0700 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return c, api.Err("configuration", "development state must be an owned, non-symlink 0700 directory")
	}
	c.AllowedUIDs = []int{os.Getuid()}
	c.ReviewerUIDs = []int{os.Getuid()}
	return c, nil
}
