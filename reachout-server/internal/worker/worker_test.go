package worker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ashmit/reachout-server/internal/campaigns"
	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/ashmit/reachout-server/internal/schedule"
)

var longAgo = utc(2026, 11, 2, 8, 0)

func TestSendsAConnectionRequestInWorkingHours(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	c := e.campaign(u, acct, "p", 1, 10, longAgo)
	w := e.worker(testConfig())

	e.tick(w, utc(2026, 11, 10, 10, 0)) // Tuesday 10:00
	if e.sentCount() != 1 {
		t.Fatalf("sent %d, want 1", e.sentCount())
	}
	s := e.sent[0]
	if s.token != "TOKEN-1" || s.action["actionType"] != "st.sendConnectionRequest" ||
		s.action["personUrl"] != leadURL("p", 0) || s.action["note"] != "Hi, let's connect" {
		t.Fatalf("workflow: %+v", s)
	}
	if got := e.attemptCounts(acct); got["submitted"] != 1 {
		t.Fatalf("attempts after submit: %v", got)
	}
	if l := e.lead(c.id, c.leads[0]); l.state != campaigns.StateQueued {
		t.Fatalf("lead moved before the result: %s", l.state)
	}

	e.tick(w, utc(2026, 11, 10, 10, 1)) // result arrives
	if l := e.lead(c.id, c.leads[0]); l.state != campaigns.StateRequestSent || l.step != 1 || l.due != nil {
		t.Fatalf("after success: %+v", l)
	}
	if got := e.attemptCounts(acct); got["succeeded"] != 1 || got["submitted"] != 0 {
		t.Fatalf("attempts: %v", got)
	}
	// A connection note is optional: an empty one is not sent at all.
	e2 := newEnv(t)
	u2 := e2.user("b@example.com")
	acct2 := e2.account(u2)
	cc, _ := e2.camps.Create(context.Background(), u2.id, campaigns.Input{AccountID: acct2, Name: "n", DailyLimit: 5,
		Steps: []campaigns.Step{{Type: campaigns.StepConnect}, steps()[1], steps()[2]}})
	e2.pool.Exec(context.Background(), `INSERT INTO leads (user_id, profile_url) VALUES ($1, 'https://www.linkedin.com/in/nonote')`, u2.id)
	var lid int64
	e2.pool.QueryRow(context.Background(), `SELECT id FROM leads WHERE user_id = $1`, u2.id).Scan(&lid)
	e2.camps.AddLeads(context.Background(), u2.id, cc.ID, []int64{lid})
	e2.camps.Start(context.Background(), u2.id, cc.ID)
	e2.setDue(cc.ID, longAgo)
	e2.tick(e2.worker(testConfig()), utc(2026, 11, 10, 10, 0))
	if _, has := e2.sent[0].action["note"]; has {
		t.Fatalf("empty note was sent: %v", e2.sent[0].action)
	}
}

func TestNothingIsSentOutsideWorkingHours(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		next time.Time // when the leads should be rescheduled to
	}{
		{"saturday", utc(2026, 11, 7, 12, 0), utc(2026, 11, 9, 9, 30)},
		{"sunday", utc(2026, 11, 8, 12, 0), utc(2026, 11, 9, 9, 30)},
		{"friday evening", utc(2026, 11, 6, 18, 0), utc(2026, 11, 9, 9, 30)},
		{"before opening", utc(2026, 11, 10, 9, 0), utc(2026, 11, 10, 9, 30)},
		{"middle of the night", utc(2026, 11, 10, 3, 0), utc(2026, 11, 10, 9, 30)},
		{"exactly at closing", utc(2026, 11, 10, 17, 30), utc(2026, 11, 11, 9, 30)},
		{"just before closing is open", utc(2026, 11, 10, 17, 29), time.Time{}}, // control: sends
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			u := e.user("a@example.com")
			acct := e.account(u)
			camp := e.campaign(u, acct, "p", 3, 10, longAgo)
			w := e.worker(testConfig())

			e.tick(w, c.at)
			if c.next.IsZero() {
				// Open, but the day's first send waits for the random start delay after 09:30.
				// 17:29 is long past it, so this one goes out.
				if e.sentCount() != 1 {
					t.Fatalf("expected a send at %v", c.at)
				}
				return
			}
			if e.sentCount() != 0 {
				t.Fatalf("sent %d actions at %v", e.sentCount(), c.at)
			}
			if got := e.attemptCounts(acct); len(got) != 0 {
				t.Fatalf("attempts recorded: %v", got)
			}
			for _, id := range camp.leads {
				if l := e.lead(camp.id, id); l.due == nil || !l.due.Equal(c.next) {
					t.Fatalf("lead rescheduled to %v, want %v", l.due, c.next)
				}
			}
			// And once the window opens it does send, after the random start delay.
			e.tick(w, c.next.Add(time.Minute))
			if e.sentCount() != 0 {
				t.Fatalf("sent before the start delay passed")
			}
			e.tick(w, c.next.Add(6*time.Minute))
			if e.sentCount() != 1 {
				t.Fatalf("sent %d after the window opened, want 1", e.sentCount())
			}
		})
	}
}

func TestHolidaysAreNotWorkingDays(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	e.caps(u, acct, 5, 10, "2026-11-10") // Tuesday is a holiday
	c := e.campaign(u, acct, "p", 2, 10, longAgo)
	w := e.worker(testConfig())

	if n := e.runDay(w, 10, 5*time.Minute); n != 0 {
		t.Fatalf("sent %d on a holiday", n)
	}
	if l := e.lead(c.id, c.leads[0]); l.due == nil || !l.due.Equal(utc(2026, 11, 11, 9, 30)) {
		t.Fatalf("rescheduled to %v, want Wednesday 09:30", l.due)
	}
	if n := e.runDay(w, 11, 5*time.Minute); n == 0 {
		t.Fatal("nothing sent the day after the holiday")
	}
}

func TestDailyAccountCap(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	e.caps(u, acct, 2, 10)
	c := e.campaign(u, acct, "p", 5, 30, longAgo)
	w := e.worker(testConfig())

	if got := e.runDay(w, 10, 4*time.Minute); got != 2 {
		t.Fatalf("Tuesday sent %d, want the cap of 2", got)
	}
	// The three left are pushed to the next working day, not retried all afternoon.
	for _, id := range c.leads[2:] {
		if l := e.lead(c.id, id); l.state != campaigns.StateQueued || l.due == nil || !l.due.Equal(utc(2026, 11, 11, 9, 30)) {
			t.Fatalf("lead after the cap: %+v", l)
		}
	}
	if got := e.runDay(w, 11, 4*time.Minute); got != 2 {
		t.Fatalf("Wednesday sent %d, want 2", got)
	}
	if got := e.runDay(w, 12, 4*time.Minute); got != 1 {
		t.Fatalf("Thursday sent %d, want the last 1", got)
	}
	if got := e.stateCounts(c.id); got[campaigns.StateRequestSent] != 5 {
		t.Fatalf("states: %v", got)
	}
}

func TestCapSpentOnFridayWaitsForMonday(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	e.caps(u, acct, 1, 10)
	c := e.campaign(u, acct, "p", 3, 30, longAgo)
	w := e.worker(testConfig())

	if got := e.runDay(w, 6, 4*time.Minute); got != 1 { // Friday
		t.Fatalf("Friday sent %d", got)
	}
	if l := e.lead(c.id, c.leads[1]); l.due == nil || !l.due.Equal(utc(2026, 11, 9, 9, 30)) {
		t.Fatalf("lead due %v, want Monday 09:30", l.due)
	}
	if got := e.runDay(w, 7, 30*time.Minute) + e.runDay(w, 8, 30*time.Minute); got != 0 {
		t.Fatalf("sent %d over the weekend", got)
	}
	if got := e.runDay(w, 9, 4*time.Minute); got != 1 {
		t.Fatalf("Monday sent %d", got)
	}
}

func TestCampaignDailyLimitIsSeparateFromTheAccountCap(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	e.caps(u, acct, 10, 10)
	a := e.campaign(u, acct, "a", 3, 1, longAgo) // at most 1 request a day
	b := e.campaign(u, acct, "b", 3, 2, longAgo) // at most 2 a day
	w := e.worker(testConfig())

	perCampaign := func(d int) map[int64]int {
		rows, _ := e.pool.Query(context.Background(),
			`SELECT campaign_id, count(*) FROM send_attempts WHERE kind = 'connect' AND created_at >= $1 AND created_at < $2 GROUP BY campaign_id`,
			day(d), day(d+1))
		defer rows.Close()
		m := map[int64]int{}
		for rows.Next() {
			var id int64
			var n int
			rows.Scan(&id, &n)
			m[id] = n
		}
		return m
	}
	e.runDay(w, 10, 4*time.Minute)
	if got := perCampaign(10); got[a.id] != 1 || got[b.id] != 2 {
		t.Fatalf("day 1: %v (campaign a=%d b=%d)", got, a.id, b.id)
	}
	e.runDay(w, 11, 4*time.Minute)
	if got := perCampaign(11); got[a.id] != 1 || got[b.id] != 1 {
		t.Fatalf("day 2: %v", got)
	}
}

func TestWarmUpRamp(t *testing.T) {
	cases := []struct {
		name      string
		rampStart string
		want      int
	}{
		{"first week", "2026-11-10", 5},
		{"second week", "2026-11-03", 10},
		{"third week", "2026-10-27", 20},
		{"after warm-up the configured cap applies", "2026-10-01", 30},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			u := e.user("a@example.com")
			acct := e.account(u)
			e.caps(u, acct, 30, 100)
			e.rampStart(acct, c.rampStart)
			e.campaign(u, acct, "p", 40, 30, longAgo)
			w := e.worker(testConfig())
			if got := e.runDay(w, 10, 4*time.Minute); got != c.want {
				t.Fatalf("sent %d, want %d", got, c.want)
			}
		})
	}
}

func TestSendsAreSpacedAndStayInsideTheWindow(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	e.campaign(u, acct, "p", 10, 30, longAgo)
	w := e.worker(testConfig())

	// 10:00 sends; the result is recorded at 10:01, so the next send is not before 10:04.
	var counts []int
	for _, m := range []int{0, 1, 2, 3, 4} {
		e.tick(w, utc(2026, 11, 10, 10, m))
		counts = append(counts, e.sentCount())
	}
	if fmt.Sprint(counts) != "[1 1 1 1 2]" {
		t.Fatalf("sends after each minute: %v, want [1 1 1 1 2]", counts)
	}

	// A send that finishes just before closing pushes the next one to tomorrow morning.
	e2 := newEnv(t)
	u2 := e2.user("b@example.com")
	acct2 := e2.account(u2)
	e2.campaign(u2, acct2, "p", 4, 30, longAgo)
	w2 := e2.worker(testConfig())
	e2.tick(w2, utc(2026, 11, 10, 17, 28))
	e2.tick(w2, utc(2026, 11, 10, 17, 29))
	var nb time.Time
	e2.pool.QueryRow(context.Background(), `SELECT next_send_not_before FROM account_schedules WHERE account_id = $1`, acct2).Scan(&nb)
	if !nb.Equal(utc(2026, 11, 11, 9, 30)) {
		t.Fatalf("not-before %v, want Wednesday 09:30", nb)
	}
}

func TestRandomGapsStayWithinBounds(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	e.campaign(u, acct, "p", 40, 30, longAgo)
	cfg := DefaultConfig() // real random gaps: 2-6 minutes, 1-10 minutes after opening
	w := e.worker(cfg)
	e.caps(u, acct, 30, 100)

	for t0 := utc(2026, 11, 10, 9, 0); t0.Before(utc(2026, 11, 10, 18, 0)); t0 = t0.Add(time.Minute) {
		e.tick(w, t0)
	}
	rows, _ := e.pool.Query(context.Background(), `SELECT created_at FROM send_attempts WHERE account_id = $1 ORDER BY id`, acct)
	var at []time.Time
	for rows.Next() {
		var c time.Time
		rows.Scan(&c)
		at = append(at, c)
	}
	rows.Close()
	if len(at) < 10 {
		t.Fatalf("only %d sends in a working day", len(at))
	}
	if first := at[0].Sub(utc(2026, 11, 10, 9, 30)); first < time.Minute || first > 11*time.Minute {
		t.Fatalf("first send %v after opening, want 1 to 10 minutes", first)
	}
	for i := 1; i < len(at); i++ {
		// 2 to 6 minutes after the previous send finished, which is up to a minute after it started.
		if gap := at[i].Sub(at[i-1]); gap < 2*time.Minute || gap > 8*time.Minute {
			t.Fatalf("gap %d is %v, want 2 to 8 minutes", i, gap)
		}
	}
	for _, a := range at {
		if a.Before(utc(2026, 11, 10, 9, 30)) || !a.Before(utc(2026, 11, 10, 17, 30)) {
			t.Fatalf("send at %v is outside the window", a)
		}
	}
}

func TestOneTaskAtATimePerAccount(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	a1, a2 := e.account(u), e.account(u)
	// Two accounts cannot be connected at once, but the second connects after the first completes.
	c1 := e.campaign(u, a1, "x", 5, 30, longAgo)
	c2 := e.campaign(u, a2, "y", 5, 30, longAgo)
	_, _ = c1, c2
	// LinkedIn takes its time: the workflows stay pending.
	e.setResults(func(sentAction) linkedin.WorkflowResult {
		return linkedin.WorkflowResult{Status: "pending", PendingReason: "outsideWorkingHours"}
	})
	w := e.worker(testConfig())

	e.tick(w, utc(2026, 11, 10, 10, 0))
	if e.sentCount() != 2 {
		t.Fatalf("each account should have started one task: %d", e.sentCount())
	}
	for _, m := range []int{5, 20, 44} {
		e.tick(w, utc(2026, 11, 10, 10, m))
	}
	if e.sentCount() != 2 {
		t.Fatalf("started more while tasks were open: %d", e.sentCount())
	}
	// After the give-up time the open requests are written off and the accounts move on.
	e.tick(w, utc(2026, 11, 10, 10, 46)) // still pending, now past the 45 minute limit
	if got := e.attemptCounts(a1); got["failed"] != 1 {
		t.Fatalf("stale attempt: %v", got)
	}
	e.setResults(func(sentAction) linkedin.WorkflowResult { return ok() })
	e.tick(w, utc(2026, 11, 10, 11, 30))
	e.tick(w, utc(2026, 11, 10, 11, 40))
	if e.sentCount() <= 2 {
		t.Fatalf("accounts did not resume after the stale attempt: %d", e.sentCount())
	}
}

func TestConcurrentWorkersNeverDoubleSend(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	e.caps(u, acct, 30, 100)
	c := e.campaign(u, acct, "p", 25, 30, longAgo)
	workers := []*Worker{e.worker(testConfig()), e.worker(testConfig()), e.worker(testConfig())}

	for round := 0; round < 40; round++ {
		at := utc(2026, 11, 10, 9, 36).Add(time.Duration(round) * 4 * time.Minute)
		e.clock.Set(at)
		before := e.sentCount()
		var wg sync.WaitGroup
		for _, w := range workers {
			wg.Add(1)
			go func(w *Worker) { defer wg.Done(); w.Tick(context.Background()) }(w)
		}
		wg.Wait()
		if d := e.sentCount() - before; d > 1 {
			t.Fatalf("round %d: %d sends started at once for one account", round, d)
		}
	}
	rows, _ := e.pool.Query(context.Background(),
		`SELECT lead_id, count(*) FROM send_attempts WHERE kind = 'connect' AND status IN ('starting','submitted','succeeded') GROUP BY lead_id HAVING count(*) > 1`)
	defer rows.Close()
	if rows.Next() {
		var lead int64
		var n int
		rows.Scan(&lead, &n)
		t.Fatalf("lead %d got %d connection requests", lead, n)
	}
	seen := map[string]bool{}
	for _, s := range e.sent {
		url := s.action["personUrl"].(string)
		if seen[url] {
			t.Fatalf("%s was sent twice", url)
		}
		seen[url] = true
	}
	if len(seen) < 20 || len(seen) != len(e.sent) {
		t.Fatalf("%d distinct sends of %d", len(seen), len(e.sent))
	}
	_ = c
}

func TestLimitExceededReschedulesInsteadOfFailing(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	c := e.campaign(u, acct, "p", 4, 30, longAgo)
	w := e.worker(testConfig())

	first := true
	e.setResults(func(sentAction) linkedin.WorkflowResult {
		if first {
			first = false
			return actionError("limitExceeded")
		}
		return ok()
	})
	e.tick(w, utc(2026, 11, 10, 10, 0))
	e.tick(w, utc(2026, 11, 10, 10, 1)) // LinkedIn says: limit reached

	l := e.lead(c.id, c.leads[0])
	if l.state != campaigns.StateQueued || l.retries != 1 || l.due == nil || !l.due.Equal(utc(2026, 11, 11, 9, 30)) {
		t.Fatalf("limited lead: %+v", l)
	}
	if got := e.attemptCounts(acct); got["failed"] != 1 {
		t.Fatalf("attempts: %v", got)
	}
	// The other leads are held back too: no more requests today.
	for _, id := range c.leads[1:] {
		if l := e.lead(c.id, id); l.due == nil || !l.due.Equal(utc(2026, 11, 11, 9, 30)) {
			t.Fatalf("other lead not held back: %+v", l)
		}
	}
	if got := e.runDay(w, 10, 4*time.Minute); got != 0 {
		t.Fatalf("sent %d more after a limit", got)
	}
	// Tomorrow it carries on, and the lead that was pushed back goes out first.
	if got := e.runDay(w, 11, 4*time.Minute); got < 4 {
		t.Fatalf("sent %d the next day, want all 4", got)
	}
	if l := e.lead(c.id, c.leads[0]); l.state != campaigns.StateRequestSent || l.retries != 0 {
		t.Fatalf("pushed-back lead after success: %+v", l)
	}
}

func TestLimitRaisedWhenStartingTheWorkflow(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	c := e.campaign(u, acct, "p", 2, 30, longAgo)
	w := e.worker(testConfig())

	e.fake.Err["StartWorkflow"] = &linkedin.Error{Type: "limitExceeded", Status: 400}
	e.tick(w, utc(2026, 11, 10, 10, 0))
	l := e.lead(c.id, c.leads[0])
	if l.state != campaigns.StateQueued || l.due == nil || !l.due.Equal(utc(2026, 11, 11, 9, 30)) {
		t.Fatalf("lead: %+v", l)
	}
	if got := e.attemptCounts(acct); got["failed"] != 1 || len(got) != 1 {
		t.Fatalf("attempts: %v", got)
	}
}

func TestAPersonLinkedInKeepsRefusingIsEventuallyFailed(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	c := e.campaign(u, acct, "p", 3, 30, longAgo)
	w := e.worker(testConfig())
	bad := leadURL("p", 0)
	e.setResults(func(a sentAction) linkedin.WorkflowResult {
		if a.action["personUrl"] == bad {
			return actionError("requestNotAllowed")
		}
		return ok()
	})
	e.runDay(w, 10, 4*time.Minute) // refused, retry 1
	e.runDay(w, 11, 4*time.Minute) // refused, retry 2
	if l := e.lead(c.id, c.leads[0]); l.state != campaigns.StateQueued || l.retries != 2 {
		t.Fatalf("after two refusals: %+v", l)
	}
	e.runDay(w, 12, 4*time.Minute) // refused, retry 3: give up on this person
	l := e.lead(c.id, c.leads[0])
	if l.state != campaigns.StateFailed || !strings.Contains(l.lastErr, "repeatedly refused") {
		t.Fatalf("after three refusals: %+v", l)
	}
	e.runDay(w, 13, 4*time.Minute)
	for _, id := range c.leads[1:] {
		if l := e.lead(c.id, id); l.state != campaigns.StateRequestSent {
			t.Fatalf("other lead %d: %+v", id, l)
		}
	}
}

func TestConnectionRequestErrorsMapToLeadStates(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	e.caps(u, acct, 30, 100)
	c := e.campaign(u, acct, "p", 6, 30, longAgo)
	w := e.worker(testConfig())

	byURL := map[string]string{
		leadURL("p", 0): "alreadyPending", leadURL("p", 1): "alreadyConnected",
		leadURL("p", 2): "invitationAlreadyReceived", leadURL("p", 3): "personNotFound",
		leadURL("p", 4): "selfProfileNotAllowed", leadURL("p", 5): "somethingNew",
	}
	e.setResults(func(a sentAction) linkedin.WorkflowResult { return actionError(byURL[a.action["personUrl"].(string)]) })
	e.runDay(w, 10, 4*time.Minute)

	want := []campaigns.State{campaigns.StateRequestSent, campaigns.StateConnected, campaigns.StateFailed,
		campaigns.StateFailed, campaigns.StateFailed, campaigns.StateFailed}
	for i, id := range c.leads {
		if l := e.lead(c.id, id); l.state != want[i] {
			t.Errorf("%s: state %s, want %s", byURL[leadURL("p", i)], l.state, want[i])
		}
	}
	if l := e.lead(c.id, c.leads[5]); !strings.Contains(l.lastErr, "somethingNew") {
		t.Errorf("unknown error not recorded: %q", l.lastErr)
	}
	// Upstream text never reaches lead data.
	if l := e.lead(c.id, c.leads[3]); strings.Contains(l.lastErr, "upstream text") {
		t.Errorf("upstream message leaked: %q", l.lastErr)
	}
	// Failures are not sent: they must not use up the cap, but successes (already pending) do.
	if got := e.attemptCounts(acct); got["succeeded"] != 2 || got["failed"] != 4 {
		t.Errorf("attempts: %v", got)
	}
}

func TestMessageAndFollowUpFlow(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	c := e.campaign(u, acct, "p", 1, 30, longAgo)
	lead := c.leads[0]
	ctx := context.Background()
	w := e.worker(testConfig())

	// The request went out and was accepted on Tuesday 10:00: the message is due Wednesday 10:00.
	e.camps.Record(ctx, c.id, lead, campaigns.EvRequestSent, utc(2026, 11, 10, 9, 50), "")
	e.camps.Record(ctx, c.id, lead, campaigns.EvAccepted, utc(2026, 11, 10, 10, 0), "")
	if l := e.lead(c.id, lead); l.due == nil || !l.due.Equal(utc(2026, 11, 11, 10, 0)) {
		t.Fatalf("message due %v", l.due)
	}
	e.tick(w, utc(2026, 11, 10, 11, 0))
	if e.sentCount() != 0 {
		t.Fatal("message sent before it was due")
	}

	e.tick(w, utc(2026, 11, 11, 10, 10))
	if s := e.sent[0]; s.action["actionType"] != "st.sendMessage" || s.action["text"] != "Thanks for connecting" || s.action["personUrl"] != leadURL("p", 0) {
		t.Fatalf("message workflow: %+v", s.action)
	}
	e.tick(w, utc(2026, 11, 11, 10, 11))
	l := e.lead(c.id, lead)
	if l.state != campaigns.StateMessaged || l.due == nil || !l.due.Equal(utc(2026, 11, 13, 10, 11)) { // +2 business days
		t.Fatalf("after message: %+v", l)
	}
	var body, dir string
	e.pool.QueryRow(ctx, `SELECT direction, body FROM messages WHERE lead_id = $1`, lead).Scan(&dir, &body)
	if dir != "out" || body != "Thanks for connecting" {
		t.Fatalf("stored message: %q %q", dir, body)
	}

	e.tick(w, utc(2026, 11, 13, 10, 30))
	if s := e.sent[1]; s.action["text"] != "Just checking in" {
		t.Fatalf("follow-up workflow: %+v", s.action)
	}
	e.tick(w, utc(2026, 11, 13, 10, 31))
	if l := e.lead(c.id, lead); l.state != campaigns.StateFollowedUp {
		t.Fatalf("after follow-up: %+v", l)
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE lead_id = $1 AND direction = 'out'`, lead).Scan(&n)
	if n != 2 {
		t.Fatalf("%d outbound messages stored, want 2", n)
	}

	// The final wait ends on a Sunday: finishing the lead sends nothing, so it needs no working hours.
	e.pool.Exec(ctx, `UPDATE campaign_leads SET next_action_at = $1 WHERE campaign_id = $2`, utc(2026, 11, 13, 11, 0), c.id)
	before := e.sentCount()
	e.tick(w, utc(2026, 11, 15, 12, 0))
	if l := e.lead(c.id, lead); l.state != campaigns.StateDone || e.sentCount() != before {
		t.Fatalf("finish on a Sunday: %+v, sends %d", l, e.sentCount()-before)
	}
}

func TestRepliedLeadsAreNeverContacted(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	c := e.campaign(u, acct, "p", 3, 30, longAgo)
	ctx := context.Background()
	e.camps.Record(ctx, c.id, c.leads[0], campaigns.EvReplied, utc(2026, 11, 10, 8, 0), "")
	e.camps.Record(ctx, c.id, c.leads[1], campaigns.EvFailed, utc(2026, 11, 10, 8, 0), "x")
	w := e.worker(testConfig())

	e.runDay(w, 10, 4*time.Minute)
	if e.sentCount() != 1 || e.sent[0].action["personUrl"] != leadURL("p", 2) {
		t.Fatalf("sent %d: %v", e.sentCount(), e.sent)
	}
}

func TestPausedStoppedAndDisconnectedAreSkipped(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	c := e.campaign(u, acct, "p", 3, 30, longAgo)
	ctx := context.Background()
	w := e.worker(testConfig())
	at := utc(2026, 11, 10, 10, 0)

	e.camps.Pause(ctx, u.id, c.id)
	e.tick(w, at)
	if e.sentCount() != 0 {
		t.Fatal("sent from a paused campaign")
	}
	e.camps.Resume(ctx, u.id, c.id)

	e.pool.Exec(ctx, `UPDATE linkedin_accounts SET status = 'reconnection_required' WHERE id = $1`, acct)
	e.setDue(c.id, longAgo)
	e.tick(w, at.Add(time.Minute))
	if e.sentCount() != 0 {
		t.Fatal("sent for an account that needs reconnecting")
	}
	e.pool.Exec(ctx, `UPDATE linkedin_accounts SET status = 'connected' WHERE id = $1`, acct)
	e.tick(w, at.Add(10*time.Minute))
	if e.sentCount() != 1 {
		t.Fatalf("did not resume once reconnected: %d", e.sentCount())
	}

	e.camps.Stop(ctx, u.id, c.id)
	e.tick(w, at.Add(11*time.Minute)) // settles the in-flight one
	e.tick(w, at.Add(20*time.Minute))
	if e.sentCount() != 1 {
		t.Fatalf("sent from a stopped campaign: %d", e.sentCount())
	}
}

func TestKillSwitch(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	c := e.campaign(u, acct, "p", 3, 30, longAgo)
	cfg := testConfig()
	cfg.Enabled = false
	w := e.worker(cfg)

	for m := 0; m < 30; m += 5 {
		e.tick(w, utc(2026, 11, 10, 10, m))
	}
	if e.sentCount() != 0 || len(e.attemptCounts(acct)) != 0 {
		t.Fatalf("a disabled worker acted: sent %d, attempts %v", e.sentCount(), e.attemptCounts(acct))
	}
	if l := e.lead(c.id, c.leads[0]); l.due == nil || !l.due.Equal(longAgo) {
		t.Fatalf("a disabled worker touched the schedule: %v", l.due)
	}
	done := make(chan struct{})
	go func() { w.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return immediately when disabled")
	}
}

func TestRestartResumesASubmittedSend(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	c := e.campaign(u, acct, "p", 2, 30, longAgo)
	e.tick(e.worker(testConfig()), utc(2026, 11, 10, 10, 0)) // first process sends, then "crashes"
	if e.sentCount() != 1 {
		t.Fatal("setup")
	}
	// A new process picks up where it left off: it collects the result and does not resend.
	second := e.worker(testConfig())
	e.tick(second, utc(2026, 11, 10, 10, 1))
	if l := e.lead(c.id, c.leads[0]); l.state != campaigns.StateRequestSent {
		t.Fatalf("result not collected after restart: %+v", l)
	}
	if e.sentCount() != 1 {
		t.Fatalf("resent after restart: %d", e.sentCount())
	}
}

func TestStaleStartingAttempts(t *testing.T) {
	ctx := context.Background()
	t.Run("a connection request is retried and the duplicate is harmless", func(t *testing.T) {
		e := newEnv(t)
		u := e.user("a@example.com")
		acct := e.account(u)
		c := e.campaign(u, acct, "p", 1, 30, longAgo)
		e.insertAttempt(u, acct, c, kindConnect, statusStarting, utc(2026, 11, 10, 9, 40))
		w := e.worker(testConfig())

		e.tick(w, utc(2026, 11, 10, 9, 44)) // under 5 minutes old: still in flight, leave it alone
		if e.sentCount() != 0 || e.attemptCounts(acct)["starting"] != 1 {
			t.Fatalf("fresh attempt was touched: %v", e.attemptCounts(acct))
		}
		e.tick(w, utc(2026, 11, 10, 10, 0))
		if got := e.attemptCounts(acct); got["failed"] != 1 || got["starting"] != 0 {
			t.Fatalf("stale attempt: %v", got)
		}
		// The first send may really have gone out; LinkedIn then says "already pending".
		e.setResults(func(sentAction) linkedin.WorkflowResult { return actionError("alreadyPending") })
		e.tick(w, utc(2026, 11, 10, 10, 30))
		e.tick(w, utc(2026, 11, 10, 10, 31))
		if l := e.lead(c.id, c.leads[0]); l.state != campaigns.StateRequestSent {
			t.Fatalf("lead: %+v", l)
		}
	})
	t.Run("a message is never resent", func(t *testing.T) {
		e := newEnv(t)
		u := e.user("a@example.com")
		acct := e.account(u)
		c := e.campaign(u, acct, "p", 1, 30, longAgo)
		e.camps.Record(ctx, c.id, c.leads[0], campaigns.EvRequestSent, utc(2026, 11, 9, 10, 0), "")
		e.camps.Record(ctx, c.id, c.leads[0], campaigns.EvAccepted, utc(2026, 11, 9, 10, 0), "")
		e.setDue(c.id, longAgo)
		e.insertAttempt(u, acct, c, kindMessage, statusStarting, utc(2026, 11, 10, 9, 40))
		w := e.worker(testConfig())

		e.tick(w, utc(2026, 11, 10, 10, 0))
		e.tick(w, utc(2026, 11, 10, 11, 0))
		l := e.lead(c.id, c.leads[0])
		if e.sentCount() != 0 || l.state != campaigns.StateFailed || !strings.Contains(l.lastErr, "outcome unknown") {
			t.Fatalf("sent %d, lead %+v", e.sentCount(), l)
		}
		if got := e.attemptCounts(acct); got["unknown"] != 1 {
			t.Fatalf("attempts: %v", got)
		}
	})
}

func (e *env) insertAttempt(u user, accountID int64, c camp, kind, status string, at time.Time) {
	e.t.Helper()
	_, err := e.pool.Exec(context.Background(),
		`INSERT INTO send_attempts (user_id, account_id, campaign_id, lead_id, kind, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`, u.id, accountID, c.id, c.leads[0], kind, status, at)
	if err != nil {
		e.t.Fatal(err)
	}
}

func TestAmbiguousStartErrors(t *testing.T) {
	ctx := context.Background()
	netErr := &linkedin.Error{Type: "network", Message: "request failed"}

	t.Run("connection request is retried later", func(t *testing.T) {
		e := newEnv(t)
		u := e.user("a@example.com")
		acct := e.account(u)
		c := e.campaign(u, acct, "p", 1, 30, longAgo)
		e.fake.Err["StartWorkflow"] = netErr
		e.tick(e.worker(testConfig()), utc(2026, 11, 10, 10, 0))
		l := e.lead(c.id, c.leads[0])
		if l.state != campaigns.StateQueued || l.due == nil || l.due.Before(utc(2026, 11, 10, 10, 5)) || l.due.After(utc(2026, 11, 10, 10, 15)) {
			t.Fatalf("lead %+v", l)
		}
		if got := e.attemptCounts(acct); got["failed"] != 1 {
			t.Fatalf("attempts: %v", got)
		}
	})
	t.Run("message is not retried", func(t *testing.T) {
		e := newEnv(t)
		u := e.user("a@example.com")
		acct := e.account(u)
		c := e.campaign(u, acct, "p", 1, 30, longAgo)
		e.camps.Record(ctx, c.id, c.leads[0], campaigns.EvRequestSent, utc(2026, 11, 9, 10, 0), "")
		e.camps.Record(ctx, c.id, c.leads[0], campaigns.EvAccepted, utc(2026, 11, 9, 10, 0), "")
		e.setDue(c.id, longAgo)
		e.fake.Err["StartWorkflow"] = &linkedin.Error{Type: "bad_response", Status: 502}
		w := e.worker(testConfig())
		e.tick(w, utc(2026, 11, 10, 10, 0))
		e.tick(w, utc(2026, 11, 10, 14, 0))
		l := e.lead(c.id, c.leads[0])
		if l.state != campaigns.StateFailed || !strings.Contains(l.lastErr, "outcome unknown") || e.sentCount() != 0 {
			t.Fatalf("lead %+v, sent %d", l, e.sentCount())
		}
	})
	t.Run("a rate limit is retried, a rejected request fails the lead", func(t *testing.T) {
		e := newEnv(t)
		u := e.user("a@example.com")
		acct := e.account(u)
		c := e.campaign(u, acct, "p", 2, 30, longAgo)
		w := e.worker(testConfig())
		e.fake.Err["StartWorkflow"] = &linkedin.Error{Type: "tooManyRequests", Status: 429}
		e.tick(w, utc(2026, 11, 10, 10, 0))
		if l := e.lead(c.id, c.leads[0]); l.state != campaigns.StateQueued {
			t.Fatalf("429 failed the lead: %+v", l)
		}
		e.fake.Err["StartWorkflow"] = &linkedin.Error{Type: "invalidWorkflow", Status: 400}
		e.tick(w, utc(2026, 11, 10, 10, 20)) // the other lead is next
		if l := e.lead(c.id, c.leads[1]); l.state != campaigns.StateFailed || !strings.Contains(l.lastErr, "invalidWorkflow") {
			t.Fatalf("400 should fail the lead: %+v", l)
		}
	})
}

func TestSignedOutAccountStopsSending(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	c := e.campaign(u, acct, "p", 3, 30, longAgo)
	e.setResults(func(sentAction) linkedin.WorkflowResult {
		return linkedin.WorkflowResult{Status: "failed", Failure: &linkedin.WorkflowFailure{Reason: "linkedinAccountSignedOut"}}
	})
	w := e.worker(testConfig())
	e.tick(w, utc(2026, 11, 10, 10, 0))
	e.tick(w, utc(2026, 11, 10, 10, 1))

	var status string
	e.pool.QueryRow(context.Background(), `SELECT status FROM linkedin_accounts WHERE id = $1`, acct).Scan(&status)
	if status != "reconnection_required" {
		t.Fatalf("account status %q", status)
	}
	if l := e.lead(c.id, c.leads[0]); l.state != campaigns.StateQueued {
		t.Fatalf("a signed-out account must not fail its leads: %+v", l)
	}
	before := e.sentCount()
	e.runDay(w, 11, 4*time.Minute)
	if e.sentCount() != before {
		t.Fatal("kept sending after the account was signed out")
	}
}

func TestEachAccountOnlyUsesItsOwnTokenAndLeads(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.user("alice@example.com"), e.user("bob@example.com")
	aAcct, bAcct := e.account(alice), e.account(bob)
	e.caps(alice, aAcct, 30, 100)
	e.caps(bob, bAcct, 30, 100)
	e.campaign(alice, aAcct, "alice", 8, 30, longAgo)
	e.campaign(bob, bAcct, "bob", 8, 30, longAgo)
	w := e.worker(testConfig())
	e.runDay(w, 10, 4*time.Minute)

	if e.sentCount() < 10 {
		t.Fatalf("only %d sends", e.sentCount())
	}
	for _, s := range e.sent {
		url := s.action["personUrl"].(string)
		switch s.token {
		case "TOKEN-1":
			if !strings.Contains(url, "/in/alice-") {
				t.Fatalf("alice's account was used for %s", url)
			}
		case "TOKEN-2":
			if !strings.Contains(url, "/in/bob-") {
				t.Fatalf("bob's account was used for %s", url)
			}
		default:
			t.Fatalf("unexpected token %q", s.token)
		}
	}
}

func TestOnlyRealSendsUseUpTheCap(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	c := e.campaign(u, acct, "p", 1, 30, longAgo)
	w := e.worker(testConfig())
	at := utc(2026, 11, 10, 12, 0)
	e.insertAttempt(u, acct, c, kindConnect, statusFailed, at)
	e.insertAttempt(u, acct, c, kindConnect, statusSucceeded, at)
	e.insertAttempt(u, acct, c, kindMessage, statusUnknown, at)
	e.insertAttempt(u, acct, c, kindConnect, statusSucceeded, utc(2026, 11, 9, 12, 0)) // yesterday

	sched, _ := e.accts.LoadSchedule(context.Background(), acct)
	from, to := sched.DayBounds(at)
	used, err := w.usedToday(context.Background(), acct, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if used[schedule.KindConnect] != 1 || used[schedule.KindMessage] != 1 {
		t.Fatalf("used today: %v, want 1 connect (failed and yesterday excluded) and 1 message (unknown counts)", used)
	}
}
