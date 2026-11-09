package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"

	"github.com/google/uuid"

	"github.com/payflow/simulator/internal/store"
)

// Market data used to make the synthetic world look like a MENA-focused PSP.
var (
	countries = []weighted[string]{
		{"EG", 45}, {"SA", 20}, {"AE", 15}, {"KW", 5}, {"DE", 10}, {"US", 5},
	}
	currencyByCountry = map[string]string{
		"EG": "EGP", "SA": "SAR", "AE": "AED", "KW": "KWD", "DE": "EUR", "US": "USD",
	}
	mccs = []weighted[string]{
		{"5411", 25}, // grocery
		{"5812", 20}, // restaurants
		{"5732", 15}, // electronics
		{"4121", 15}, // ride hailing / taxi
		{"5999", 20}, // misc retail
		{"7995", 5},  // gambling: high risk
	}
	brands = []weighted[string]{{"visa", 45}, {"mastercard", 40}, {"meeza", 10}, {"amex", 5}}
	psps   = []weighted[string]{{"adyen", 40}, {"checkout", 35}, {"paymob", 25}}

	nameA = []string{"Nile", "Delta", "Cairo", "Gulf", "Desert", "Pyramid", "Falcon", "Oasis", "Lotus", "Atlas"}
	nameB = []string{"Market", "Foods", "Electronics", "Rides", "Pharmacy", "Fashion", "Books", "Coffee", "Games", "Travel"}
)

// GenerateActors builds the reference data deterministically from r.
func GenerateActors(r *rand.Rand, nMerchants, nCustomers int) ([]store.Merchant, []store.Customer, []store.Instrument) {
	merchants := make([]store.Merchant, 0, nMerchants)
	for i := 0; i < nMerchants; i++ {
		country := pick(r, countries)
		mcc := pick(r, mccs)
		tier := "standard"
		switch {
		case mcc == "7995":
			tier = "high"
		case r.Float64() < 0.2:
			tier = "low"
		}
		merchants = append(merchants, store.Merchant{
			ID:                 newID(r),
			LegalName:          fmt.Sprintf("%s %s %03d", nameA[r.IntN(len(nameA))], nameB[r.IntN(len(nameB))], i),
			CountryCode:        country,
			MCC:                mcc,
			SettlementCurrency: currencyByCountry[country],
			RiskTier:           tier,
		})
	}

	customers := make([]store.Customer, 0, nCustomers)
	instruments := make([]store.Instrument, 0, nCustomers*3/2)
	for i := 0; i < nCustomers; i++ {
		c := store.Customer{
			ID:          newID(r),
			EmailSHA256: sha(fmt.Sprintf("customer%06d@example.com", i)),
			CountryCode: pick(r, countries),
		}
		customers = append(customers, c)

		cards := 1 + r.IntN(2) // 1 or 2 cards per customer
		for j := 0; j < cards; j++ {
			brand := pick(r, brands)
			issuing := c.CountryCode
			if r.Float64() < 0.05 { // some people hold foreign-issued cards
				issuing = pick(r, countries)
			}
			id := newID(r)
			instruments = append(instruments, store.Instrument{
				ID:             id,
				CustomerID:     c.ID,
				CardBrand:      brand,
				BIN:            bin(r, brand),
				Last4:          fmt.Sprintf("%04d", r.IntN(10000)),
				IssuingCountry: issuing,
				Fingerprint:    sha(id.String()),
			})
		}
	}
	return merchants, customers, instruments
}

// bin returns a plausible 6-digit Bank Identification Number for the brand.
func bin(r *rand.Rand, brand string) string {
	switch brand {
	case "visa":
		return fmt.Sprintf("4%05d", r.IntN(100000))
	case "mastercard":
		return fmt.Sprintf("5%d%04d", 1+r.IntN(5), r.IntN(10000))
	case "amex":
		return fmt.Sprintf("3%d%04d", []int{4, 7}[r.IntN(2)], r.IntN(10000))
	default: // meeza (Egypt's domestic scheme)
		return "507803"
	}
}

// newID makes a UUID from our seeded RNG so that SIM_SEED reproduces the same
// merchants and customers. (Runtime IDs use time-ordered UUIDv7 instead.)
func newID(r *rand.Rand) uuid.UUID {
	var b [16]byte
	for i := range b {
		b[i] = byte(r.UintN(256))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return uuid.UUID(b)
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

type weighted[T any] struct {
	v T
	w int
}

func pick[T any](r *rand.Rand, items []weighted[T]) T {
	total := 0
	for _, it := range items {
		total += it.w
	}
	n := r.IntN(total)
	for _, it := range items {
		if n < it.w {
			return it.v
		}
		n -= it.w
	}
	return items[len(items)-1].v
}
