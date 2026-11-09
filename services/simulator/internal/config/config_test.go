package config

import (
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:secret@localhost:5432/payflow")
	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.TPS != 5 || c.Merchants != 50 || c.FeeBps != 250 {
		t.Errorf("unexpected defaults: %+v", c)
	}
}

func TestValidationCollectsAllErrors(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("SIM_TPS", "-1")
	t.Setenv("SIM_FRAUD_RATE", "2")
	_, err := Load()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"DATABASE_URL", "SIM_TPS", "SIM_FRAUD_RATE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s, got: %v", want, err)
		}
	}
}

func TestRedactedHidesPassword(t *testing.T) {
	c := Config{DatabaseURL: "postgres://app:supersecret@postgres:5432/payflow"}
	got := c.Redacted().DatabaseURL
	if strings.Contains(got, "supersecret") {
		t.Fatalf("password leaked: %s", got)
	}
	if got != "postgres://app:****@postgres:5432/payflow" {
		t.Errorf("unexpected redaction: %s", got)
	}
}
