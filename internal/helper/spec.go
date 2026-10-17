package helper

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/agyard/agsudo/internal/api"
)

var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{0,128}$`)
var termPattern = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,128}$`)

func lookupUser(name string) (api.Identity, error) {
	var u *user.User
	var err error
	if name == "" {
		name = "root"
	}
	if strings.HasPrefix(name, "#") {
		u, err = user.LookupId(strings.TrimPrefix(name, "#"))
	} else {
		u, err = user.Lookup(name)
	}
	if err != nil {
		return api.Identity{}, api.Err("invalid_user", "%v", err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return api.Identity{}, err
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return api.Identity{}, err
	}
	groups, err := u.GroupIds()
	if err != nil {
		return api.Identity{}, err
	}
	x := api.Identity{UID: uint32(uid), GID: uint32(gid), Name: u.Username, Home: u.HomeDir, Shell: "/bin/sh", Groups: []uint32{}}
	for _, g := range groups {
		n, e := strconv.ParseUint(g, 10, 32)
		if e != nil {
			return x, e
		}
		x.Groups = append(x.Groups, uint32(n))
	}
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return x, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) == 7 && f[2] == u.Uid && f[6] != "" {
			x.Shell = f[6]
			break
		}
	}
	return x, nil
}

func executableFile(path string) (*os.File, string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", err
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, "", err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 || st.Size() > 256<<20 {
		f.Close()
		return nil, "", api.Err("invalid_command", "executable must be a regular executable file no larger than 256 MiB")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (256<<20)+1))
	if err != nil {
		f.Close()
		return nil, "", err
	}
	if n > 256<<20 {
		f.Close()
		return nil, "", api.Err("invalid_command", "executable exceeds 256 MiB")
	}
	if _, err = f.Seek(0, 0); err != nil {
		f.Close()
		return nil, "", err
	}
	return f, hex.EncodeToString(h.Sum(nil)), nil
}
func resolve(name, cwd, path string) (string, error) {
	var candidates []string
	if strings.ContainsRune(name, '/') {
		if !filepath.IsAbs(name) {
			name = filepath.Join(cwd, name)
		}
		candidates = []string{name}
	} else {
		for _, dir := range strings.Split(path, ":") {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}
	for _, p := range candidates {
		p, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		st, err := os.Stat(p)
		if err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0111 != 0 {
			return p, nil
		}
	}
	return "", api.Err("command_not_found", "executable %q not found in the configured secure PATH", name)
}

func prepare(in api.Input, c Config, uid, gid int) (api.Spec, error) {
	s := api.Spec{Argv: in.Argv, Cwd: in.Cwd, Reason: in.Reason, Context: in.Context, Session: in.Session, Scope: "command", Login: in.Login}
	if !filepath.IsAbs(s.Cwd) || strings.IndexByte(s.Cwd, 0) >= 0 {
		return s, api.Err("invalid_input", "cwd must be an absolute path")
	}
	if len(in.Reason) > 8192 || !sessionPattern.MatchString(in.Session) {
		return s, api.Err("invalid_input", "invalid reason or session")
	}
	if in.Term != "" && !termPattern.MatchString(in.Term) {
		return s, api.Err("invalid_input", "invalid TERM")
	}
	var err error
	s.Target, err = lookupUser(in.User)
	if err != nil {
		return s, err
	}
	if c.Dev && int(s.Target.UID) != uid {
		return s, api.Err("dev_target", "development mode can only execute as UID %d; use -u '#%d'", uid, uid)
	}
	if in.Shell || in.Login {
		if len(s.Argv) > 0 {
			return s, api.Err("invalid_input", "-s/-i do not accept command arguments; use an explicit shell -c command")
		}
		s.Argv = []string{s.Target.Shell}
		s.Scope = "shell_session"
	}
	if len(s.Argv) == 0 || len(s.Argv) > 256 {
		return s, api.Err("invalid_input", "specify 1 to 256 command arguments")
	}
	total := 0
	for _, arg := range s.Argv {
		total += len(arg)
		if strings.IndexByte(arg, 0) >= 0 {
			return s, api.Err("invalid_input", "arguments cannot contain NUL")
		}
	}
	if total > 65536 {
		return s, api.Err("invalid_input", "command arguments exceed 64 KiB")
	}
	s.Executable, err = resolve(s.Argv[0], s.Cwd, c.SecurePath)
	if err != nil {
		return s, err
	}
	f, digest, err := executableFile(s.Executable)
	if err != nil {
		return s, err
	}
	f.Close()
	s.ExecutableSHA256 = digest
	if len(s.Argv) == 1 {
		switch filepath.Base(s.Executable) {
		case "bash", "sh", "dash", "zsh", "fish", "ksh":
			s.Scope = "shell_session"
		}
	}
	if in.Login {
		s.Cwd = s.Target.Home
	}
	if st, err := os.Stat(s.Cwd); err != nil || !st.IsDir() {
		return s, api.Err("invalid_input", "cwd is not an existing directory")
	}
	if len(in.Context) > 8 {
		return s, api.Err("invalid_input", "at most 8 context files")
	}
	total = 0
	for i, f := range s.Context {
		total += len(f.Content)
		if !filepath.IsAbs(f.Path) || len(f.Content) > 65536 || !utf8.ValidString(f.Content) || api.Hash([]byte(f.Content)) != f.SHA256 {
			return s, api.Err("invalid_input", "invalid context snapshot")
		}
		s.Context[i].Source = "caller_supplied"
	}
	if total > 256<<10 {
		return s, api.Err("invalid_input", "context exceeds 256 KiB")
	}
	caller, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return s, err
	}
	s.Environment = []string{"PATH=" + c.SecurePath, "HOME=" + s.Target.Home, "USER=" + s.Target.Name, "LOGNAME=" + s.Target.Name, "SHELL=" + s.Target.Shell, "LANG=C.UTF-8", fmt.Sprintf("SUDO_UID=%d", uid), fmt.Sprintf("SUDO_GID=%d", gid), "SUDO_USER=" + caller.Username}
	s.Environment = append(s.Environment, "SUDO_COMMAND="+strings.Join(s.Argv, " "))
	if in.Term != "" {
		s.Environment = append(s.Environment, "TERM="+in.Term)
	}
	return s, nil
}
