// Package payment defines the payment lifecycle as an explicit state machine.
// Encoding the allowed transitions in one place means the simulator (and later
// the data quality tests) can reject impossible histories such as failed->captured.
package payment

import "fmt"

type Status string

const (
	StatusCreated    Status = "created"
	StatusAuthorized Status = "authorized"
	StatusCaptured   Status = "captured"
	StatusFailed     Status = "failed"
	StatusVoided     Status = "voided"
)

// transitions lists, for each status, the statuses it may move to.
// Terminal statuses (captured, failed, voided) have no entry.
var transitions = map[Status][]Status{
	StatusCreated:    {StatusAuthorized, StatusFailed},
	StatusAuthorized: {StatusCaptured, StatusVoided},
}

// CanTransition reports whether from -> to is a legal move.
func CanTransition(from, to Status) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// IsTerminal reports whether a payment in this status can no longer change.
func IsTerminal(s Status) bool {
	_, ok := transitions[s]
	return !ok
}

// ErrInvalidTransition is returned when code asks for an illegal move.
type ErrInvalidTransition struct{ From, To Status }

func (e ErrInvalidTransition) Error() string {
	return fmt.Sprintf("invalid payment transition %s -> %s", e.From, e.To)
}

// Failure reasons as returned by card networks (simplified).
var FailureReasons = []string{
	"insufficient_funds", "do_not_honor", "expired_card", "suspected_fraud", "invalid_cvv",
}
