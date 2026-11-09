package money

import (
	"math/rand/v2"
	"testing"
)

func TestFormat(t *testing.T) {
	tests := []struct {
		amount   int64
		currency string
		want     string
	}{
		{123456, "EGP", "1234.56 EGP"},
		{5, "USD", "0.05 USD"},
		{1500, "KWD", "1.500 KWD"},
		{5000, "JPY", "5000 JPY"},
		{-250, "EUR", "-2.50 EUR"},
	}
	for _, tt := range tests {
		if got := Format(tt.amount, tt.currency); got != tt.want {
			t.Errorf("Format(%d, %s) = %q, want %q", tt.amount, tt.currency, got, tt.want)
		}
	}
}

func TestFeeRoundsHalfUp(t *testing.T) {
	tests := []struct{ amount, bps, want int64 }{
		{10000, 250, 250}, // 2.5% of 100.00 = 2.50
		{101, 250, 3},     // 2.525 -> 3 (half-up)
		{99, 250, 2},      // 2.475 -> 2
		{1, 250, 0},
	}
	for _, tt := range tests {
		if got := Fee(tt.amount, tt.bps); got != tt.want {
			t.Errorf("Fee(%d, %d) = %d, want %d", tt.amount, tt.bps, got, tt.want)
		}
	}
}

func TestRandomAmountIsPositiveAndDeterministic(t *testing.T) {
	r1 := rand.New(rand.NewPCG(42, 42))
	r2 := rand.New(rand.NewPCG(42, 42))
	for i := 0; i < 1000; i++ {
		a, err := RandomAmount(r1, "KWD", i%2 == 0)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := RandomAmount(r2, "KWD", i%2 == 0)
		if a < 1 {
			t.Fatalf("amount must be >= 1 minor unit, got %d", a)
		}
		if a != b {
			t.Fatalf("same seed must give same amounts: %d != %d", a, b)
		}
	}
}

func TestUnknownCurrency(t *testing.T) {
	if _, err := MinorUnits("XXX"); err == nil {
		t.Fatal("expected error for unknown currency")
	}
}
