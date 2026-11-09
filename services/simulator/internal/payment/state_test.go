package payment

import "testing"

func TestTransitions(t *testing.T) {
	tests := []struct {
		from, to Status
		want     bool
	}{
		{StatusCreated, StatusAuthorized, true},
		{StatusCreated, StatusFailed, true},
		{StatusAuthorized, StatusCaptured, true},
		{StatusAuthorized, StatusVoided, true},
		{StatusCreated, StatusCaptured, false}, // must authorize first
		{StatusFailed, StatusCaptured, false},  // terminal
		{StatusCaptured, StatusVoided, false},  // too late: refund instead
		{StatusVoided, StatusAuthorized, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.from)+"->"+string(tt.to), func(t *testing.T) {
			if got := CanTransition(tt.from, tt.to); got != tt.want {
				t.Errorf("CanTransition = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTerminal(t *testing.T) {
	for _, s := range []Status{StatusCaptured, StatusFailed, StatusVoided} {
		if !IsTerminal(s) {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []Status{StatusCreated, StatusAuthorized} {
		if IsTerminal(s) {
			t.Errorf("%s should not be terminal", s)
		}
	}
}
