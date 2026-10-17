package helper

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/agyard/agsudo/internal/api"
)

func TestProtocolRejectsUnknownFieldsAndTrailingMessages(t *testing.T) {
	for _, in := range []string{`{"operation":"get","uid":0}`, `{"operation":"get"} {}`, strings.Repeat("x", api.MaxMessage+1)} {
		if _, err := decode(strings.NewReader(in)); err == nil {
			t.Fatalf("accepted invalid protocol: %.80s", in)
		}
	}
}
func TestExecutableRejectsSpecialAndSymlinkFiles(t *testing.T) {
	if f, _, err := executableFile("/dev/null"); err == nil {
		f.Close()
		t.Fatal("accepted device")
	}
	d := t.TempDir()
	file := filepath.Join(d, "script")
	if err := os.WriteFile(file, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(d, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if f, _, err := executableFile(link); err == nil {
		f.Close()
		t.Fatal("accepted final symlink")
	}
	f, hash, err := executableFile(file)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if len(hash) != 64 {
		t.Fatal("bad digest")
	}
}
func TestContextAndEnvironment(t *testing.T) {
	c := defaults()
	uid := os.Getuid()
	in := api.Input{Argv: []string{"id"}, User: "#" + strconv.Itoa(uid), Cwd: t.TempDir(), Context: []api.File{{Path: "/example", Content: "data"}}}
	spec, err := prepare(in, c, uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")
	env, err := environment(api.Request{OwnerUID: uid, Spec: spec}, c.SecurePath, "xterm-256color\nX=1")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range env {
		if strings.HasPrefix(v, "LD_") || strings.HasPrefix(v, "TERM=") || strings.HasPrefix(v, "X=") {
			t.Fatalf("unsafe environment entry %q", v)
		}
	}
	in.Context[0].Content = "\xff"
	if _, err = prepare(in, c, uid); err == nil {
		t.Fatal("invalid UTF-8 context accepted")
	}
}
