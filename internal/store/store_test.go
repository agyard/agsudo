package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agyard/agsudo/internal/api"
)

var testContext = context.Background()
var owner = Access{UID: 1000, Reviewer: true}

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.Alive = func(api.Process) bool { return true }
	return s, path
}
func create(t *testing.T, s *Store) api.Request {
	t.Helper()
	r, err := s.Create(testContext, api.Request{ID: api.ID("req_"), OwnerUID: 1000, ExpiresAt: s.Now().Add(time.Hour), Spec: api.Spec{Argv: []string{"id", api.ID("")}, Session: "test-session"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func approve(t *testing.T, s *Store, r api.Request) api.Request {
	t.Helper()
	out, err := s.Decide(testContext, r.ID, owner, api.Decision{Action: "approve"}, Check{Digest: r.Digest})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func code(t *testing.T, err error, want string) {
	t.Helper()
	var e *api.Error
	if !errors.As(err, &e) || e.Code != want {
		t.Fatalf("got %v, want error code %s", err, want)
	}
}

func TestOneShotExecutionAndAudit(t *testing.T) {
	s, _ := testStore(t)
	r := create(t, s)
	r = approve(t, s, r)
	if _, err := s.Decide(testContext, r.ID, owner, api.Decision{Action: "approve"}, Check{Digest: r.Digest}); err != nil {
		t.Fatal(err)
	}
	p := api.Process{PID: 123, Start: "456", Boot: "test"}
	if _, err := s.ClaimExecution(testContext, r.ID, owner, p); err != nil {
		t.Fatal(err)
	}
	_, err := s.ClaimExecution(testContext, r.ID, owner, p)
	code(t, err, "running")
	if _, err = s.Started(testContext, r.ID, owner, p, 124); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Finish(testContext, r.ID, owner, p, 42, ""); err != nil {
		t.Fatal(err)
	}
	_, err = s.ClaimExecution(testContext, r.ID, owner, p)
	code(t, err, "exited")
	got, err := s.Get(testContext, r.ID, owner)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExitCode == nil || *got.ExitCode != 42 || got.State != "exited" {
		t.Fatalf("bad result: %+v", got)
	}
	page, err := s.Events(testContext, owner, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"request.created", "request.approved", "execution.claimed", "execution.started", "execution.exited"}
	if len(page.Events) != len(want) {
		t.Fatalf("events: %+v", page.Events)
	}
	for i, e := range page.Events {
		if e.Type != want[i] || e.Sequence != int64(i+1) || e.ActorUID != 1000 {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
}

func TestConcurrentFirstOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	start, results := make(chan struct{}), make(chan error, 16)
	for range cap(results) {
		go func() {
			<-start
			s, err := Open(path)
			if err != nil {
				results <- err
				return
			}
			defer s.Close()
			_, err = s.Create(testContext, api.Request{ID: api.ID("req_"), OwnerUID: 1000, ExpiresAt: time.Now().Add(time.Minute), Spec: api.Spec{Argv: []string{api.ID("")}}}, false)
			results <- err
		}()
	}
	close(start)
	for range cap(results) {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.Events(testContext, owner, "", "", 0)
	if err != nil || len(p.Events) != cap(results) {
		t.Fatalf("lost requests during initialization: %d events, %v", len(p.Events), err)
	}
}

func TestCancelCompetesWithExecutionAcrossConnections(t *testing.T) {
	s, path := testStore(t)
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.Alive = s.Alive
	for i := 0; i < 24; i++ {
		r := approve(t, s, create(t, s))
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var cancelErr, claimErr error
		go func() { defer wg.Done(); <-start; _, cancelErr = s.Cancel(testContext, r.ID, owner, "canceled") }()
		go func() {
			defer wg.Done()
			<-start
			_, claimErr = other.ClaimExecution(testContext, r.ID, owner, api.Process{PID: 1})
		}()
		close(start)
		wg.Wait()
		if (cancelErr == nil) == (claimErr == nil) {
			t.Fatalf("exactly one must win: cancel=%v claim=%v", cancelErr, claimErr)
		}
		got, e := s.Get(testContext, r.ID, owner)
		if e != nil {
			t.Fatal(e)
		}
		if cancelErr == nil && got.State != "canceled" {
			t.Fatalf("cancelled request ran: %s", got.State)
		}
		if claimErr == nil && got.State != "running" {
			t.Fatalf("claimed request was canceled: %s", got.State)
		}
	}
}

func TestLateApprovalAndDigestMismatch(t *testing.T) {
	s, _ := testStore(t)
	r := create(t, s)
	_, err := s.Decide(testContext, r.ID, owner, api.Decision{Action: "approve"}, Check{Digest: "wrong"})
	code(t, err, "conflict")
	_, err = s.Cancel(testContext, r.ID, owner, "no longer needed")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Decide(testContext, r.ID, owner, api.Decision{Action: "approve"}, Check{Digest: r.Digest})
	code(t, err, "conflict")
	r = create(t, s)
	now := r.ExpiresAt
	s.Now = func() time.Time { return now }
	_, err = s.Decide(testContext, r.ID, owner, api.Decision{Action: "approve"}, Check{Digest: r.Digest})
	code(t, err, "conflict")
	got, err := s.Get(testContext, r.ID, owner)
	if err != nil || got.State != "expired" {
		t.Fatalf("expiry did not commit: %+v %v", got, err)
	}
}

func TestAccessAndReviewerCannotExecuteSomeoneElsesRequest(t *testing.T) {
	s, _ := testStore(t)
	r := create(t, s)
	_, err := s.Get(testContext, r.ID, Access{UID: 1001})
	code(t, err, "not_found")
	_, err = s.Decide(testContext, r.ID, Access{UID: 1000}, api.Decision{Action: "approve"}, Check{})
	code(t, err, "forbidden")
	r, err = s.Decide(testContext, r.ID, Access{UID: 1001, Reviewer: true}, api.Decision{Action: "approve"}, Check{Digest: r.Digest})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ClaimExecution(testContext, r.ID, Access{UID: 1001, Reviewer: true}, api.Process{PID: 1})
	code(t, err, "forbidden")
	p, err := s.Events(testContext, Access{UID: 1001}, "", "", 0)
	if err != nil || len(p.Events) != 0 {
		t.Fatalf("cross-user event leak: %+v %v", p, err)
	}
}

func TestDeferKeepsSuggestionAndCrashNeverRetries(t *testing.T) {
	s, _ := testStore(t)
	r := create(t, s)
	r, err := s.Decide(testContext, r.ID, owner, api.Decision{Action: "defer", Reason: "need a person", Recommend: "approve"}, Check{Digest: r.Digest})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.List(testContext, owner, []string{"pending"}, "")
	if err != nil || len(p) != 0 {
		t.Fatalf("deferred request recirculated: %+v %v", p, err)
	}
	r = approve(t, s, r)
	if r.Suggestion == nil || r.Suggestion.Reason != "need a person" {
		t.Fatal("suggestion lost")
	}
	_, err = s.ClaimExecution(testContext, r.ID, owner, api.Process{PID: 1})
	if err != nil {
		t.Fatal(err)
	}
	s.Alive = func(api.Process) bool { return false }
	got, err := s.Get(testContext, r.ID, owner)
	if err != nil || got.State != "unknown" {
		t.Fatalf("dead monitor must be unknown: %+v %v", got, err)
	}
	_, err = s.ClaimExecution(testContext, r.ID, owner, api.Process{PID: 2})
	code(t, err, "unknown")
}

func TestAuditFailureRollsBackApproval(t *testing.T) {
	s, _ := testStore(t)
	r := create(t, s)
	_, err := s.db.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'simulated audit failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Decide(testContext, r.ID, owner, api.Decision{Action: "approve"}, Check{Digest: r.Digest})
	if err == nil {
		t.Fatal("approved despite audit failure")
	}
	got, err := s.Get(testContext, r.ID, owner)
	if err != nil || got.State != "pending" {
		t.Fatalf("state escaped transaction: %+v %v", got, err)
	}
}

func TestOnDemandExpiryAndQueueQuota(t *testing.T) {
	s, _ := testStore(t)
	r := create(t, s)
	now := r.ExpiresAt
	s.Now = func() time.Time { return now }
	page, err := s.Events(testContext, owner, "", r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Events[1].Type != "request.expired" {
		t.Fatalf("missing expiry event: %+v", page)
	}
	// Expired requests in other sessions do not count against the quota.
	for range 256 {
		if _, err = s.Create(testContext, api.Request{ID: api.ID("req_"), OwnerUID: 1000, ExpiresAt: now.Add(time.Second), Spec: api.Spec{Argv: []string{api.ID("")}, Session: "a"}}, false); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.Create(testContext, api.Request{ID: api.ID("req_"), OwnerUID: 1000, ExpiresAt: now.Add(time.Hour), Spec: api.Spec{Argv: []string{"another"}}}, false)
	code(t, err, "queue_full")
	now = now.Add(time.Second)
	if _, err = s.Create(testContext, api.Request{ID: api.ID("req_"), OwnerUID: 1000, ExpiresAt: now.Add(time.Hour), Spec: api.Spec{Session: "b"}}, false); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatedCommandReusesRequestAndGuardsDecisions(t *testing.T) {
	s, _ := testStore(t)
	submit := func(spec api.Spec, reuseOnly bool) (api.Request, error) {
		return s.Create(testContext, api.Request{ID: api.ID("req_"), OwnerUID: 1000, ExpiresAt: s.Now().Add(time.Hour), Spec: spec}, reuseOnly)
	}
	r := create(t, s)
	plain := r.Spec
	if again, err := submit(plain, false); err != nil || again.ID != r.ID {
		t.Fatalf("same command created another request: %s %v", again.ID, err)
	}
	// A new reason replaces the pending request instead of duplicating it;
	// a bare retry or sudo -n then attaches to the explained request.
	explained := r.Spec
	explained.Reason = "added a reason after reading the help"
	again, err := submit(explained, false)
	if err != nil || again.ID == r.ID {
		t.Fatalf("new reason was dropped: %v", err)
	}
	if old, _ := s.Get(testContext, r.ID, owner); old.State != "canceled" || old.Failure != "replaced by "+again.ID || again.Prior.ID != r.ID {
		t.Fatalf("replaced request still pending: %+v", old)
	}
	for _, reuseOnly := range []bool{false, true} {
		if bare, err := submit(plain, reuseOnly); err != nil || bare.ID != again.ID {
			t.Fatalf("bare retry (reuseOnly=%v) did not attach: %s %v", reuseOnly, bare.ID, err)
		}
	}
	r = again
	_, err = s.Decide(testContext, r.ID, owner, api.Decision{Action: "approve"}, Check{Session: r.Spec.Session})
	code(t, err, "forbidden")
	if _, err = s.Decide(testContext, r.ID, owner, api.Decision{Action: "defer"}, Check{}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Decide(testContext, r.ID, owner, api.Decision{Action: "approve"}, Check{PendingOnly: true})
	code(t, err, "conflict")
	r = approve(t, s, r)
	// An approval is reused whatever the reason, also without creating.
	if again, err = submit(plain, true); err != nil || again.ID != r.ID {
		t.Fatalf("approval not reused: %v", err)
	}
	if _, err = s.ClaimExecution(testContext, r.ID, owner, api.Process{PID: 1}); err != nil {
		t.Fatal(err)
	}
	_, err = submit(r.Spec, true)
	code(t, err, "pending")
	if again, err = submit(r.Spec, false); err != nil || again.ID == r.ID {
		t.Fatalf("a used request was reused: %v", err)
	}
	// The identical request gets a recent denial again; a revised reason or a
	// later attempt asks anew.
	denied := create(t, s)
	if _, err = s.Decide(testContext, denied.ID, owner, api.Decision{Action: "deny"}, Check{}); err != nil {
		t.Fatal(err)
	}
	if again, err = submit(denied.Spec, false); err != nil || again.ID != denied.ID || again.State != "denied" {
		t.Fatalf("denial not reported: %+v %v", again, err)
	}
	revised := denied.Spec
	revised.Reason = "answering the reviewer"
	if again, err = submit(revised, false); err != nil || again.ID == denied.ID || again.Prior.State != "denied" {
		t.Fatalf("denial blocked or hid a revised request: %+v %v", again, err)
	}
	if _, err = s.Cancel(testContext, again.ID, owner, "test"); err != nil {
		t.Fatal(err)
	}
	later := denied.ExpiresAt
	s.Now = func() time.Time { return later }
	if again, err = submit(denied.Spec, false); err != nil || again.ID == denied.ID {
		t.Fatalf("old denial blocked a new request: %v", err)
	}
}

func TestNewerSchemaRejected(t *testing.T) {
	s, path := testStore(t)
	if _, err := s.db.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	other, err := Open(path)
	if err == nil {
		other.Close()
		t.Fatal("opened and downgraded a future schema")
	}
	code(t, err, "schema_version")
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 99 {
		t.Fatalf("schema changed: %d %v", version, err)
	}
}
