// Package campaigns holds outreach campaigns: the three-step sequence, the per-lead state
// machine, and the lifecycle (start, pause, resume, stop).
package campaigns

// State is where one lead stands in a campaign.
//
//	queued -> request_sent -> connected -> messaged -> followed_up -> done
//	any non-final state -> replied | failed          done -> replied (late reply)
type State string

const (
	StateQueued      State = "queued"
	StateRequestSent State = "request_sent"
	StateConnected   State = "connected"
	StateMessaged    State = "messaged"
	StateFollowedUp  State = "followed_up"
	StateReplied     State = "replied"
	StateDone        State = "done"
	StateFailed      State = "failed"
)

// Event is something that happened to a lead, from our worker or from Linked API.
type Event string

const (
	EvRequestSent  Event = "request_sent"
	EvAccepted     Event = "accepted"
	EvMessageSent  Event = "message_sent"
	EvFollowUpSent Event = "followup_sent"
	EvReplied      Event = "replied"
	EvDone         Event = "done"   // follow-up wait finished with no reply
	EvFailed       Event = "failed" // permanent error for this lead
)

// Outcome says what Apply decided.
type Outcome int

const (
	// Applied: the state changes to the returned one.
	Applied Outcome = iota
	// Noop: the event is a duplicate, late, or arrived after a terminal state. It is safe
	// to drop; nothing changes and no error should be raised.
	Noop
	// Invalid: the event cannot happen from this state (for example a message sent before
	// the connection was accepted). That points at a bug or corrupt data.
	Invalid
)

// rank orders the forward path. Terminal states are handled separately.
func rank(s State) int {
	switch s {
	case StateQueued:
		return 0
	case StateRequestSent:
		return 1
	case StateConnected:
		return 2
	case StateMessaged:
		return 3
	case StateFollowedUp:
		return 4
	}
	return -1
}

// IsTerminal reports whether a lead has finished (done is terminal but can still turn
// into replied if the person answers late).
func IsTerminal(s State) bool {
	return s == StateReplied || s == StateDone || s == StateFailed
}

// Apply is the only place that decides state changes. It is pure and tolerates events in
// any order: webhooks can arrive late, twice, or before our own bookkeeping, so an event
// that is behind the current state is a Noop, never a move backwards.
func Apply(cur State, ev Event) (State, Outcome) {
	switch ev {
	case EvReplied:
		switch cur {
		case StateReplied, StateFailed:
			return cur, Noop
		}
		return StateReplied, Applied // includes done -> replied
	case EvFailed:
		if IsTerminal(cur) {
			return cur, Noop
		}
		return StateFailed, Applied
	}

	if IsTerminal(cur) {
		return cur, Noop
	}
	r := rank(cur)
	if r < 0 {
		return cur, Invalid // unknown state
	}

	switch ev {
	case EvRequestSent:
		if r == 0 {
			return StateRequestSent, Applied
		}
		return cur, Noop
	case EvAccepted:
		// May also arrive while we still think the request is only queued (we sent it,
		// then crashed before recording it): skip forward.
		if r <= 1 {
			return StateConnected, Applied
		}
		return cur, Noop
	case EvMessageSent:
		switch {
		case r == 2:
			return StateMessaged, Applied
		case r >= 3:
			return cur, Noop
		}
		return cur, Invalid
	case EvFollowUpSent:
		switch {
		case r == 3:
			return StateFollowedUp, Applied
		case r >= 4:
			return cur, Noop
		}
		return cur, Invalid
	case EvDone:
		if r == 4 {
			return StateDone, Applied
		}
		return cur, Invalid
	}
	return cur, Invalid
}

// stepReached is the value stored in campaign_leads.current_step for a state: the furthest
// stage the lead has been through. It never decreases, which makes the funnel counts
// (sent, accepted, messaged, followed up) simple and stable even after a reply or failure.
// nextStep is the value to store after moving to state s: the larger of what was already
// reached and what s implies, so a reply after a message keeps the message's step.
func nextStep(stored int, s State) int {
	if r := stepReached(s); r > stored {
		return r
	}
	return stored
}

func stepReached(s State) int {
	switch s {
	case StateRequestSent:
		return 1
	case StateConnected, StateReplied: // a reply means the person is connected
		return 2
	case StateMessaged:
		return 3
	case StateFollowedUp, StateDone:
		return 4
	}
	return 0
}
