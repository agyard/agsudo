package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/thezzisu/agsudo/internal/api"
)

func TestHelpAndUsageDoNotOpenHelper(t *testing.T) {
	t.Setenv("AGSUDO_HELPER", "/does/not/exist")
	for _, args := range [][]string{{"--help"}, {"review", "--help"}, {"help", "audit", "export"}, {"help", "requester"}, {"version", "--json"}} {
		var out, errOut bytes.Buffer
		if code := Execute(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 0 {
			t.Fatalf("%v: %d %s", args, code, errOut.String())
		}
		if out.Len() == 0 {
			t.Fatalf("no help for %v", args)
		}
	}
	for _, args := range [][]string{{"nonsense"}, {"approve"}, {"exec"}, {"exec", "--bad-flag"}, {"help", "nonsense"}, {"review", "--tui", "--json"}, {"exec", "--request", "req_x", "--", "id"}} {
		var out, errOut bytes.Buffer
		if code := Execute(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 2 {
			t.Fatalf("%v: %d %s", args, code, errOut.String())
		}
	}
}
func TestSanitizeUntrustedTerminalContent(t *testing.T) {
	in := "safe\x1b[2J\x00\r\u202e\ntext"
	got := safe(in, false)
	for _, r := range []rune{27, 0, 13, 0x202e, 10} {
		if strings.ContainsRune(got, r) {
			t.Fatalf("control rune survived: %q", got)
		}
	}
	if !strings.Contains(safe(in, true), "\ntext") {
		t.Fatal("lost file line breaks")
	}
}

type fakeReview struct{ last api.Input }

func (f *fakeReview) ListAll(context.Context, []string, string) ([]api.Request, error) {
	return nil, nil
}
func (f *fakeReview) Call(_ context.Context, in api.Input, out any) error {
	f.last = in
	if p, ok := out.(*api.Request); ok {
		*p = api.Request{ID: in.ID, State: "approved"}
	}
	return nil
}
func TestTUIConfirmationPinsReviewedRequest(t *testing.T) {
	f := &fakeReview{}
	m := newReviewModel(context.Background(), f, []string{"pending"}, "")
	r := api.Request{ID: "req_first", State: "pending", Digest: "first-digest", ExpiresAt: time.Now().Add(time.Hour), Spec: api.Spec{Argv: []string{"id"}}}
	m.Update(refreshed{requests: []api.Request{r}, generation: 0})
	m.Update(tea.KeyPressMsg{Code: 'a'})
	if m.confirm == nil || m.confirm.ID != r.ID {
		t.Fatal("did not enter confirmation")
	}
	other := r
	other.ID = "req_other"
	other.Digest = "other-digest"
	m.Update(refreshed{requests: []api.Request{other}, generation: 0})
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msg := cmd()
	if _, ok := msg.(decided); !ok {
		t.Fatalf("unexpected decision message %T", msg)
	}
	if f.last.ID != r.ID || f.last.ExpectDigest != r.Digest {
		t.Fatalf("refresh changed confirmed command: %+v", f.last)
	}
}
