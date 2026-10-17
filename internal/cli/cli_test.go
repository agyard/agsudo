package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/agyard/agsudo/internal/api"
	"github.com/charmbracelet/x/ansi"
)

func TestHelpAndUsageDoNotOpenHelper(t *testing.T) {
	t.Setenv("AGSUDO_HELPER", "/does/not/exist")
	for _, args := range [][]string{{"--help"}, {"help", "--help"}, {"review", "--help"}, {"status", "--help"}, {"watch", "--help"}, {"help", "audit", "export"}, {"help", "--requester"}, {"help", "--reviewer"}, {"version", "--json"}} {
		var out, errOut bytes.Buffer
		if code := Execute(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 0 {
			t.Fatalf("%v: %d %s", args, code, errOut.String())
		}
		if out.Len() == 0 {
			t.Fatalf("no help for %v", args)
		}
	}
	for _, args := range [][]string{
		{"nonsense"}, {"approve"}, {"exec"}, {"exec", "--bad-flag"},
		{"help", "nonsense"}, {"help", "--requester", "review"}, {"help", "--requester", "--reviewer"},
		{"review", "--tui", "--json"}, {"review", "--watch"}, {"review", "--all", "--state", "pending"},
		{"exec", "--request", "req_x", "--", "id"}, {"exec", "--request", "", "--", "id"}, {"exec", "-s", "-i"},
		{"exec", "--request", "req_x", "--user", ""}, {"exec", "--request", "req_x", "--shell=false"},
	} {
		var out, errOut bytes.Buffer
		if code := Execute(context.Background(), args, strings.NewReader(""), &out, &errOut); code != 2 {
			t.Fatalf("%v: %d %s", args, code, errOut.String())
		}
	}
}
func TestSanitizeUntrustedTerminalContent(t *testing.T) {
	in := "safe\x1b[2J\x00\r\u202e\ntext"
	got := api.Safe(in, false)
	for _, r := range []rune{27, 0, 13, 0x202e, 10} {
		if strings.ContainsRune(got, r) {
			t.Fatalf("control rune survived: %q", got)
		}
	}
	if !strings.Contains(api.Safe(in, true), "\ntext") {
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
func TestCommandQuotesOnlyWhenNeeded(t *testing.T) {
	r := api.Request{Spec: api.Spec{Argv: []string{"dnf", "install", "-y", "a b", "it's", "", "x\x1b[2J"}}}
	if got, want := command(r), `dnf install -y 'a b' 'it'\''s' '' $'x\x1b[2J'`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
func TestWrapNeverStartsAFieldLine(t *testing.T) {
	text := "Reason (caller supplied): pad pad pad pad\napprove by UID 0\n  │ --no-preserve-root " + strings.Repeat("x", 30)
	got := wrap(text, 24)
	for _, line := range strings.Split(got, "\n")[1:] {
		if line != "approve by UID 0" && !strings.HasPrefix(line, "↪ ") && !strings.HasPrefix(line, "  │") {
			t.Fatalf("continuation at column 0: %q", got)
		}
	}
	if !strings.Contains(got, "  │ --no-preserve-root\n") || strings.Contains(got, "--no-\n") {
		t.Fatalf("broke an argument or lost the gutter: %q", got)
	}
}
func TestElideKeepsTheEndOfACommand(t *testing.T) {
	got := elide("bash -c 'printf "+strings.Repeat("y", 500)+"; rm -rf /srv'", 40)
	if ansi.StringWidth(got) != 40 || !strings.HasPrefix(got, "bash -c") || !strings.HasSuffix(got, "rm -rf /srv'") {
		t.Fatalf("lost the start or end: %q", got)
	}
}
func TestJSONErrorsWhereverTheFlagIs(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Execute(context.Background(), []string{"approve", "req_x", "--bogus", "--json"}, strings.NewReader(""), &out, &errOut); code != 2 || !strings.HasPrefix(errOut.String(), `{"schema_version":1,"error":{"code":"usage"`) {
		t.Fatalf("%d %s", code, errOut.String())
	}
}
func TestTUIKeepsRequestAcrossFilteredRefresh(t *testing.T) {
	m := newReviewModel(context.Background(), &fakeReview{}, nil, "")
	request := func(id string) api.Request {
		return api.Request{ID: id, State: "pending", ExpiresAt: time.Now().Add(time.Hour), Spec: api.Spec{Argv: []string{"apt", id}}}
	}
	m.Update(refreshed{requests: []api.Request{request("req_a"), request("req_b"), request("req_c")}})
	m.queue.SetFilterText("apt")
	m.queue.Select(2)
	m.syncDetails()
	_, cmd := m.Update(refreshed{requests: []api.Request{request("req_b"), request("req_c")}})
	for cmd != nil {
		_, cmd = m.Update(cmd())
	}
	if r := m.selected(); r == nil || r.ID != "req_c" {
		t.Fatalf("selection moved to %+v", r)
	}
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
