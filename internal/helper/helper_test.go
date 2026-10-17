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
	name := "#" + strconv.Itoa(uid)
	in := api.Input{Argv: []string{"id"}, User: name, Cwd: t.TempDir(), Term: "xterm-256color", Context: []api.File{{Path: "/example", Content: "data"}}}
	s, err := prepare(in, c, uid, os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range s.Environment {
		if strings.HasPrefix(v, "LD_") || strings.HasPrefix(v, "AGSUDO_HELPER=") {
			t.Fatal("unsafe inherited environment")
		}
	}
	if s.Context[0].Source != "caller_supplied" || s.Context[0].SHA256 != api.Hash([]byte("data")) {
		t.Fatal("context attribution missing")
	}
	in.Context[0].Content = "\xff"
	if _, err = prepare(in, c, uid, os.Getgid()); err == nil {
		t.Fatal("invalid UTF-8 context accepted")
	}
}
