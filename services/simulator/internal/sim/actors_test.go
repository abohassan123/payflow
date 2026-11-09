package sim

import (
	"math/rand/v2"
	"testing"
)

func TestGenerateActorsIsDeterministicAndValid(t *testing.T) {
	m1, c1, i1 := GenerateActors(rand.New(rand.NewPCG(7, 7)), 20, 100)
	m2, _, i2 := GenerateActors(rand.New(rand.NewPCG(7, 7)), 20, 100)

	if len(m1) != 20 || len(c1) != 100 {
		t.Fatalf("got %d merchants, %d customers", len(m1), len(c1))
	}
	if len(i1) < 100 || len(i1) > 200 {
		t.Fatalf("expected 1-2 instruments per customer, got %d", len(i1))
	}
	if m1[0].ID != m2[0].ID || i1[len(i1)-1].ID != i2[len(i2)-1].ID {
		t.Fatal("same seed must produce the same actors")
	}

	seen := map[string]bool{}
	for _, in := range i1 {
		if len(in.BIN) != 6 || len(in.Last4) != 4 || len(in.Fingerprint) != 64 {
			t.Errorf("bad instrument: %+v", in)
		}
		if seen[in.Fingerprint] {
			t.Errorf("duplicate fingerprint %s", in.Fingerprint)
		}
		seen[in.Fingerprint] = true
	}
	for _, m := range m1 {
		if m.MCC == "7995" && m.RiskTier != "high" {
			t.Errorf("gambling merchant must be high risk: %+v", m)
		}
		if _, ok := currencyByCountry[m.CountryCode]; !ok {
			t.Errorf("merchant country without currency: %s", m.CountryCode)
		}
	}
}

func TestPickRespectsWeights(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	items := []weighted[string]{{"a", 90}, {"b", 10}}
	counts := map[string]int{}
	for i := 0; i < 10000; i++ {
		counts[pick(r, items)]++
	}
	if counts["a"] < 8500 || counts["a"] > 9500 {
		t.Errorf("weight 90 should give ~9000 of 10000, got %d", counts["a"])
	}
}
