package campaigns

import "testing"

var allStates = []State{StateQueued, StateRequestSent, StateConnected, StateMessaged, StateFollowedUp, StateReplied, StateDone, StateFailed}
var allEvents = []Event{EvRequestSent, EvAccepted, EvMessageSent, EvFollowUpSent, EvReplied, EvDone, EvFailed}

func TestHappyPath(t *testing.T) {
	cur := StateQueued
	for _, step := range []struct {
		ev   Event
		want State
	}{
		{EvRequestSent, StateRequestSent},
		{EvAccepted, StateConnected},
		{EvMessageSent, StateMessaged},
		{EvFollowUpSent, StateFollowedUp},
		{EvDone, StateDone},
	} {
		next, out := Apply(cur, step.ev)
		if out != Applied || next != step.want {
			t.Fatalf("%s + %s = %s (%v), want %s", cur, step.ev, next, out, step.want)
		}
		cur = next
	}
}

func TestReplyStopsTheSequenceFromAnyStep(t *testing.T) {
	for _, s := range []State{StateQueued, StateRequestSent, StateConnected, StateMessaged, StateFollowedUp, StateDone} {
		if next, out := Apply(s, EvReplied); out != Applied || next != StateReplied {
			t.Errorf("replied from %s: %s %v", s, next, out)
		}
	}
	// Once replied, nothing moves it, including a duplicate reply and later sends.
	for _, ev := range allEvents {
		if next, out := Apply(StateReplied, ev); out != Noop || next != StateReplied {
			t.Errorf("replied + %s = %s %v, want unchanged Noop", ev, next, out)
		}
	}
}

func TestFailedIsFinal(t *testing.T) {
	for _, ev := range allEvents {
		if next, out := Apply(StateFailed, ev); out != Noop || next != StateFailed {
			t.Errorf("failed + %s = %s %v", ev, next, out)
		}
	}
	for _, s := range []State{StateQueued, StateRequestSent, StateConnected, StateMessaged, StateFollowedUp} {
		if next, out := Apply(s, EvFailed); out != Applied || next != StateFailed {
			t.Errorf("failed from %s: %s %v", s, next, out)
		}
	}
	// A lead that already finished does not become failed afterwards.
	if _, out := Apply(StateDone, EvFailed); out != Noop {
		t.Error("done + failed should be a no-op")
	}
}

func TestDoneOnlyTurnsIntoReplied(t *testing.T) {
	for _, ev := range []Event{EvRequestSent, EvAccepted, EvMessageSent, EvFollowUpSent, EvDone, EvFailed} {
		if next, out := Apply(StateDone, ev); out != Noop || next != StateDone {
			t.Errorf("done + %s = %s %v", ev, next, out)
		}
	}
	if next, out := Apply(StateDone, EvReplied); out != Applied || next != StateReplied {
		t.Errorf("late reply: %s %v", next, out)
	}
}

// Duplicates and late events must never move a lead backwards or change anything.
func TestStaleAndDuplicateEventsAreIgnored(t *testing.T) {
	cases := []struct {
		cur State
		ev  Event
	}{
		{StateRequestSent, EvRequestSent},
		{StateConnected, EvRequestSent},
		{StateConnected, EvAccepted},
		{StateMessaged, EvRequestSent},
		{StateMessaged, EvAccepted},
		{StateMessaged, EvMessageSent},
		{StateFollowedUp, EvAccepted},
		{StateFollowedUp, EvMessageSent},
		{StateFollowedUp, EvFollowUpSent},
	}
	for _, c := range cases {
		if next, out := Apply(c.cur, c.ev); out != Noop || next != c.cur {
			t.Errorf("%s + %s = %s %v, want unchanged Noop", c.cur, c.ev, next, out)
		}
	}
}

// Acceptance may be reported before we recorded the request: skip forward is allowed.
func TestAcceptanceBeforeRequestRecorded(t *testing.T) {
	if next, out := Apply(StateQueued, EvAccepted); out != Applied || next != StateConnected {
		t.Fatalf("queued + accepted = %s %v", next, out)
	}
	// And a late "request sent" after the acceptance is just dropped.
	if next, out := Apply(StateConnected, EvRequestSent); out != Noop || next != StateConnected {
		t.Fatalf("connected + request_sent = %s %v", next, out)
	}
}

func TestImpossibleEventsAreInvalid(t *testing.T) {
	for _, c := range []struct {
		cur State
		ev  Event
	}{
		{StateQueued, EvMessageSent},
		{StateRequestSent, EvMessageSent},
		{StateQueued, EvFollowUpSent},
		{StateRequestSent, EvFollowUpSent},
		{StateConnected, EvFollowUpSent},
		{StateQueued, EvDone},
		{StateMessaged, EvDone},
		{State("bogus"), EvAccepted},
		{StateQueued, Event("bogus")},
	} {
		if next, out := Apply(c.cur, c.ev); out != Invalid || next != c.cur {
			t.Errorf("%s + %s = %s %v, want unchanged Invalid", c.cur, c.ev, next, out)
		}
	}
}

// Whatever the input, Apply never returns a state with a lower rank than the one it was
// given unless it is moving to a terminal state; the step reached never decreases.
func TestNeverMovesBackwards(t *testing.T) {
	for _, cur := range allStates {
		for _, ev := range allEvents {
			next, out := Apply(cur, ev)
			if out != Applied {
				if next != cur {
					t.Errorf("%s + %s: %v changed the state to %s", cur, ev, out, next)
				}
				continue
			}
			if !IsTerminal(next) && rank(next) <= rank(cur) {
				t.Errorf("%s + %s moved backwards or sideways to %s", cur, ev, next)
			}
			if nextStep(stepReached(cur), next) < stepReached(cur) {
				t.Errorf("%s + %s lowered the step reached", cur, ev)
			}
			if IsTerminal(cur) && !(cur == StateDone && next == StateReplied) {
				t.Errorf("terminal %s moved to %s on %s", cur, next, ev)
			}
		}
	}
}

// Any ordering of the events that really happened must end in the same place as the
// in-order sequence, once duplicates and stale ones are dropped.
func TestOutOfOrderDeliveryConverges(t *testing.T) {
	run := func(events []Event) State {
		cur := StateQueued
		for _, ev := range events {
			if next, out := Apply(cur, ev); out == Applied {
				cur = next
			}
		}
		return cur
	}
	if got := run([]Event{EvAccepted, EvRequestSent, EvAccepted, EvMessageSent, EvMessageSent}); got != StateMessaged {
		t.Errorf("got %s", got)
	}
	if got := run([]Event{EvRequestSent, EvReplied, EvAccepted, EvMessageSent, EvFollowUpSent}); got != StateReplied {
		t.Errorf("reply must win over later sends: got %s", got)
	}
	if got := run([]Event{EvRequestSent, EvAccepted, EvMessageSent, EvFollowUpSent, EvDone, EvReplied}); got != StateReplied {
		t.Errorf("late reply after done: got %s", got)
	}
}

func TestNextStepKeepsTheFurthest(t *testing.T) {
	if got := nextStep(3, StateReplied); got != 3 {
		t.Errorf("reply after message: %d, want 3", got)
	}
	if got := nextStep(1, StateReplied); got != 2 {
		t.Errorf("reply right after request: %d, want 2 (a reply implies connected)", got)
	}
	if got := nextStep(2, StateFailed); got != 2 {
		t.Errorf("failure must not lower the step: %d", got)
	}
	if got := nextStep(0, StateRequestSent); got != 1 {
		t.Errorf("first send: %d", got)
	}
}

func TestStepReached(t *testing.T) {
	want := map[State]int{StateQueued: 0, StateRequestSent: 1, StateConnected: 2, StateReplied: 2,
		StateMessaged: 3, StateFollowedUp: 4, StateDone: 4, StateFailed: 0}
	for s, w := range want {
		if got := stepReached(s); got != w {
			t.Errorf("stepReached(%s) = %d, want %d", s, got, w)
		}
	}
}
