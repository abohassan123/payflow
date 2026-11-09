// Package money holds the rules for representing amounts.
//
// Golden rule of payments: never store or add money as float64. Floats cannot
// represent 0.10 exactly, so sums drift. We store integers in the currency's
// minor unit (EGP 12.50 -> 1250 piasters). Floats appear here only to GENERATE
// random realistic amounts, and are rounded to an integer immediately.
package money

import (
	"fmt"
	"math"
	"math/rand/v2"
)

// minorUnits mirrors the payments.currencies reference table.
var minorUnits = map[string]int{
	"EGP": 2, "USD": 2, "EUR": 2, "SAR": 2, "AED": 2, "KWD": 3, "JPY": 0,
}

// typicalBasket is the median purchase in MAJOR units, per currency.
var typicalBasket = map[string]float64{
	"EGP": 750, "USD": 40, "EUR": 35, "SAR": 150, "AED": 150, "KWD": 12, "JPY": 5000,
}

// MinorUnits returns how many decimals a currency has.
func MinorUnits(currency string) (int, error) {
	u, ok := minorUnits[currency]
	if !ok {
		return 0, fmt.Errorf("unknown currency %q", currency)
	}
	return u, nil
}

// Supported reports whether we know the currency.
func Supported(currency string) bool {
	_, ok := minorUnits[currency]
	return ok
}

// RandomAmount draws a log-normal purchase amount (many small, few large
// purchases - like real shopping) and returns it in minor units.
// highValue multiplies the median, used by fraud scenarios.
func RandomAmount(r *rand.Rand, currency string, highValue bool) (int64, error) {
	units, err := MinorUnits(currency)
	if err != nil {
		return 0, err
	}
	median := typicalBasket[currency]
	if highValue {
		median *= 6
	}
	major := median * math.Exp(r.NormFloat64()*0.8)
	minor := int64(math.Round(major * math.Pow10(units)))
	if minor < 1 {
		minor = 1
	}
	return minor, nil
}

// Fee returns the processing fee for an amount, in basis points
// (1 bp = 0.01%), rounded half-up, using integer math only.
func Fee(amountMinor int64, bps int64) int64 {
	return (amountMinor*bps + 5000) / 10000
}

// Format renders minor units for humans/logs: Format(123456, "EGP") = "1234.56 EGP".
func Format(amountMinor int64, currency string) string {
	units, err := MinorUnits(currency)
	if err != nil {
		return fmt.Sprintf("%d (minor) %s", amountMinor, currency)
	}
	if units == 0 {
		return fmt.Sprintf("%d %s", amountMinor, currency)
	}
	sign := ""
	if amountMinor < 0 {
		sign, amountMinor = "-", -amountMinor
	}
	div := int64(math.Pow10(units))
	return fmt.Sprintf("%s%d.%0*d %s", sign, amountMinor/div, units, amountMinor%div, currency)
}
