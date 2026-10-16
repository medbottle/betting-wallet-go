package money

import (
	"encoding/json"
	"fmt"
)

// Currency is an ISO 4217 code from the allow-list. The zero value is
// invalid. Every supported currency has scale 2.
type Currency struct {
	code string
}

// Supported currencies. USD exists only to exercise currency mismatch
// (ARCHITECTURE §2).
var (
	BRL = Currency{code: "BRL"}
	USD = Currency{code: "USD"}
)

var allowed = map[string]Currency{"BRL": BRL, "USD": USD}

// ParseCurrency accepts exactly three uppercase ASCII letters naming an
// allowed currency. A malformed code (e.g. "brl") is ErrInvalidMoney; a
// well-formed code outside the allow-list is ErrUnsupportedCurrency.
func ParseCurrency(s string) (Currency, error) {
	if len(s) != 3 {
		return Currency{}, fmt.Errorf("%w: currency %q must be 3 uppercase letters", ErrInvalidMoney, s)
	}
	for i := 0; i < 3; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return Currency{}, fmt.Errorf("%w: currency %q must be 3 uppercase letters", ErrInvalidMoney, s)
		}
	}
	c, ok := allowed[s]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrUnsupportedCurrency, s)
	}
	return c, nil
}

// Valid reports whether c is an allowed currency.
func (c Currency) Valid() bool {
	_, ok := allowed[c.code]
	return ok
}

// String returns the ISO 4217 code, or "" for the zero value.
func (c Currency) String() string { return c.code }

// MarshalJSON writes the code as a JSON string.
func (c Currency) MarshalJSON() ([]byte, error) {
	if !c.Valid() {
		return nil, fmt.Errorf("%w: cannot marshal uninitialised currency", ErrInvalidMoney)
	}
	return json.Marshal(c.code)
}
