package money

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// Sentinel errors. Callers classify with errors.Is.
var (
	// ErrInvalidMoney covers malformed amounts, malformed currency codes and
	// uninitialised values.
	ErrInvalidMoney = errors.New("money: invalid")
	// ErrUnsupportedCurrency means a well-formed code outside the allow-list.
	ErrUnsupportedCurrency = errors.New("money: unsupported currency")
	// ErrCurrencyMismatch means arithmetic or comparison across currencies.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	// ErrOverflow means the result does not fit in int64 minor units.
	ErrOverflow = errors.New("money: overflow")
)

// Scale is the number of decimal places of every supported currency.
const Scale = 2

// MaxExternalMinor is the largest amount accepted from external input:
// 999,999,999,999.99 (ARCHITECTURE §2). Internal values use the full int64
// range.
const MaxExternalMinor int64 = 99_999_999_999_999

// maxIntegerDigits bounds the integer part of an external amount.
const maxIntegerDigits = 12

// Money is an immutable amount in minor units of a currency. The zero value
// is invalid and rejected by every operation.
type Money struct {
	amount   int64
	currency Currency
}

// Parse builds Money from external input. amount must be in canonical form:
// no sign, no leading zeros, exactly two decimals, at most twelve integer
// digits. There is no normalisation, so the accepted string is exactly
// String() of the result.
func Parse(amount, currency string) (Money, error) {
	c, err := ParseCurrency(currency)
	if err != nil {
		return Money{}, err
	}
	minor, err := parseAmount(amount)
	if err != nil {
		return Money{}, err
	}
	return Money{amount: minor, currency: c}, nil
}

// parseAmount walks the digits by hand; floating point is never involved.
func parseAmount(s string) (int64, error) {
	dot := len(s) - Scale - 1
	if dot < 1 || s[dot] != '.' {
		return 0, fmt.Errorf("%w: amount %q must have exactly %d decimal places", ErrInvalidMoney, s, Scale)
	}
	intPart, fracPart := s[:dot], s[dot+1:]
	if len(intPart) > maxIntegerDigits {
		return 0, fmt.Errorf("%w: amount %q exceeds %d integer digits", ErrInvalidMoney, s, maxIntegerDigits)
	}
	if len(intPart) > 1 && intPart[0] == '0' {
		return 0, fmt.Errorf("%w: amount %q has leading zeros", ErrInvalidMoney, s)
	}
	var minor int64
	for _, part := range []string{intPart, fracPart} {
		for i := 0; i < len(part); i++ {
			d := part[i]
			if d < '0' || d > '9' {
				return 0, fmt.Errorf("%w: amount %q is not a plain decimal", ErrInvalidMoney, s)
			}
			// Checked multiply-add: the digit limit already rules out
			// overflow, but parsing never relies on that alone.
			if minor > (math.MaxInt64-int64(d-'0'))/10 {
				return 0, fmt.Errorf("%w: amount %q", ErrOverflow, s)
			}
			minor = minor*10 + int64(d-'0')
		}
	}
	return minor, nil
}

// FromMinor builds Money from minor units, for rehydration and internal
// calculations. Any int64 is accepted, including negatives.
func FromMinor(minor int64, c Currency) (Money, error) {
	if !c.Valid() {
		return Money{}, fmt.Errorf("%w: uninitialised currency", ErrInvalidMoney)
	}
	return Money{amount: minor, currency: c}, nil
}

// Zero returns zero in currency c.
func Zero(c Currency) (Money, error) {
	return FromMinor(0, c)
}

// Valid reports whether m was built by a constructor.
func (m Money) Valid() bool { return m.currency.Valid() }

// Minor returns the amount in minor units.
func (m Money) Minor() int64 { return m.amount }

// Currency returns the currency.
func (m Money) Currency() Currency { return m.currency }

// IsZero reports whether m is a valid zero amount.
func (m Money) IsZero() bool { return m.Valid() && m.amount == 0 }

// IsPositive reports whether m is a valid amount greater than zero.
func (m Money) IsPositive() bool { return m.Valid() && m.amount > 0 }

// IsNegative reports whether m is a valid amount below zero.
func (m Money) IsNegative() bool { return m.Valid() && m.amount < 0 }

// Add returns m + o.
func (m Money) Add(o Money) (Money, error) {
	if err := m.compatible(o); err != nil {
		return Money{}, err
	}
	if (o.amount > 0 && m.amount > math.MaxInt64-o.amount) ||
		(o.amount < 0 && m.amount < math.MinInt64-o.amount) {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrOverflow, m, o)
	}
	return Money{amount: m.amount + o.amount, currency: m.currency}, nil
}

// Sub returns m - o.
func (m Money) Sub(o Money) (Money, error) {
	if err := m.compatible(o); err != nil {
		return Money{}, err
	}
	if (o.amount < 0 && m.amount > math.MaxInt64+o.amount) ||
		(o.amount > 0 && m.amount < math.MinInt64+o.amount) {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrOverflow, m, o)
	}
	return Money{amount: m.amount - o.amount, currency: m.currency}, nil
}

// Neg returns -m. Negating math.MinInt64 overflows.
func (m Money) Neg() (Money, error) {
	if !m.Valid() {
		return Money{}, fmt.Errorf("%w: uninitialised value", ErrInvalidMoney)
	}
	if m.amount == math.MinInt64 {
		return Money{}, fmt.Errorf("%w: -(%s)", ErrOverflow, m)
	}
	return Money{amount: -m.amount, currency: m.currency}, nil
}

// Cmp returns -1, 0 or +1 as m is less than, equal to or greater than o.
func (m Money) Cmp(o Money) (int, error) {
	if err := m.compatible(o); err != nil {
		return 0, err
	}
	switch {
	case m.amount < o.amount:
		return -1, nil
	case m.amount > o.amount:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equal reports whether m and o are valid and identical.
func (m Money) Equal(o Money) bool {
	return m.Valid() && o.Valid() && m == o
}

func (m Money) compatible(o Money) error {
	if !m.Valid() || !o.Valid() {
		return fmt.Errorf("%w: uninitialised value", ErrInvalidMoney)
	}
	if m.currency != o.currency {
		return fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	return nil
}

// String returns the decimal amount with exactly two places, e.g. "25.00"
// or "-0.50". It is the canonical form used in JSON and in payload hashes.
func (m Money) String() string {
	if !m.Valid() {
		return "<invalid money>"
	}
	neg := m.amount < 0
	// uint64 holds |math.MinInt64| without overflow.
	abs := uint64(m.amount)
	if neg {
		abs = -abs
	}
	const unit = 100
	s := strconv.FormatUint(abs/unit, 10) + "." + fmt.Sprintf("%02d", abs%unit)
	if neg {
		return "-" + s
	}
	return s
}

type wireMoney struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// MarshalJSON writes {"amount":"25.00","currency":"BRL"}. Negative values
// are written with a sign (e.g. a reconciliation difference).
func (m Money) MarshalJSON() ([]byte, error) {
	if !m.Valid() {
		return nil, fmt.Errorf("%w: cannot marshal uninitialised value", ErrInvalidMoney)
	}
	return json.Marshal(wireMoney{Amount: m.String(), Currency: m.currency.String()})
}

// UnmarshalJSON applies the external input rules: amount must be a JSON
// string in canonical form (a JSON number is rejected, so no float ever
// touches the value), currency must be in the allow-list.
func (m *Money) UnmarshalJSON(b []byte) error {
	var raw struct {
		Amount   json.RawMessage `json:"amount"`
		Currency json.RawMessage `json:"currency"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMoney, err)
	}
	amount, err := jsonString(raw.Amount, "amount")
	if err != nil {
		return err
	}
	currency, err := jsonString(raw.Currency, "currency")
	if err != nil {
		return err
	}
	parsed, err := Parse(amount, currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

func jsonString(raw json.RawMessage, field string) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return "", fmt.Errorf("%w: %s must be a JSON string", ErrInvalidMoney, field)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrInvalidMoney, field, err)
	}
	return s, nil
}
