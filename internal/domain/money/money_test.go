package money

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func mustParse(t *testing.T, amount, currency string) Money {
	t.Helper()
	m, err := Parse(amount, currency)
	if err != nil {
		t.Fatalf("Parse(%q, %q): %v", amount, currency, err)
	}
	return m
}

func mustMinor(t *testing.T, minor int64, c Currency) Money {
	t.Helper()
	m, err := FromMinor(minor, c)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseValid(t *testing.T) {
	tests := []struct {
		in    string
		minor int64
	}{
		{"0.00", 0},
		{"0.01", 1},
		{"0.50", 50},
		{"1.00", 100},
		{"25.00", 2500},
		{"1000.00", 100000},
		{"999999999999.99", MaxExternalMinor},
	}
	for _, tt := range tests {
		m := mustParse(t, tt.in, "BRL")
		if m.Minor() != tt.minor || m.Currency() != BRL {
			t.Errorf("Parse(%q) = %d %s, want %d BRL", tt.in, m.Minor(), m.Currency(), tt.minor)
		}
		if m.String() != tt.in {
			t.Errorf("String() = %q, want %q (no normalisation)", m.String(), tt.in)
		}
	}
}

func TestParseRejects(t *testing.T) {
	tests := []string{
		"", " ", "1", "25", "25.5", "25.000", ".50", "1.", "-1.00", "+1.00", "-0.00",
		" 1.00", "1.00 ", "01.00", "00.00", "1,00", "1_000.00", "1e3", "1.5e2", "1E+02",
		"NaN", "nan", "Infinity", "-Infinity", "inf", "0x10.00", "١.٠٠", "1.0O",
		"1000000000000.00", "9999999999999.99", "99999999999999999999.00",
	}
	for _, in := range tests {
		if m, err := Parse(in, "BRL"); !errors.Is(err, ErrInvalidMoney) {
			t.Errorf("Parse(%q) = %v, %v; want ErrInvalidMoney", in, m, err)
		}
	}
}

func TestParseCurrency(t *testing.T) {
	for _, in := range []string{"BRL", "USD"} {
		if _, err := ParseCurrency(in); err != nil {
			t.Errorf("ParseCurrency(%q): %v", in, err)
		}
	}
	for _, in := range []string{"", "brl", "Brl", "BR", "BRLL", "B1L", " BRL"} {
		if _, err := ParseCurrency(in); !errors.Is(err, ErrInvalidMoney) {
			t.Errorf("ParseCurrency(%q) = %v, want ErrInvalidMoney", in, err)
		}
	}
	for _, in := range []string{"EUR", "XXX", "JPY"} {
		if _, err := ParseCurrency(in); !errors.Is(err, ErrUnsupportedCurrency) {
			t.Errorf("ParseCurrency(%q) = %v, want ErrUnsupportedCurrency", in, err)
		}
	}
	if _, err := Parse("1.00", "EUR"); !errors.Is(err, ErrUnsupportedCurrency) {
		t.Errorf("Parse with EUR = %v, want ErrUnsupportedCurrency", err)
	}
}

func TestZeroValueRejected(t *testing.T) {
	var z Money
	one := mustParse(t, "1.00", "BRL")
	if z.Valid() || z.IsZero() || z.IsPositive() || z.IsNegative() {
		t.Error("zero value must not report as valid")
	}
	if _, err := z.Add(one); !errors.Is(err, ErrInvalidMoney) {
		t.Errorf("Add = %v", err)
	}
	if _, err := one.Sub(z); !errors.Is(err, ErrInvalidMoney) {
		t.Errorf("Sub = %v", err)
	}
	if _, err := z.Neg(); !errors.Is(err, ErrInvalidMoney) {
		t.Errorf("Neg = %v", err)
	}
	if _, err := z.Cmp(one); !errors.Is(err, ErrInvalidMoney) {
		t.Errorf("Cmp = %v", err)
	}
	if _, err := json.Marshal(z); !errors.Is(err, ErrInvalidMoney) {
		t.Errorf("Marshal = %v", err)
	}
	if z.Equal(z) {
		t.Error("zero values must not be Equal")
	}
	if _, err := FromMinor(1, Currency{}); !errors.Is(err, ErrInvalidMoney) {
		t.Errorf("FromMinor with zero currency = %v", err)
	}
	if _, err := Zero(Currency{}); !errors.Is(err, ErrInvalidMoney) {
		t.Errorf("Zero with zero currency = %v", err)
	}
}

func TestArithmetic(t *testing.T) {
	a := mustParse(t, "100.00", "BRL")
	b := mustParse(t, "80.00", "BRL")

	sum, err := a.Add(b)
	if err != nil || sum.String() != "180.00" {
		t.Errorf("Add = %v, %v", sum, err)
	}
	diff, err := b.Sub(a)
	if err != nil || diff.String() != "-20.00" || !diff.IsNegative() {
		t.Errorf("Sub = %v, %v", diff, err)
	}
	neg, err := a.Neg()
	if err != nil || neg.String() != "-100.00" {
		t.Errorf("Neg = %v, %v", neg, err)
	}
	if c, err := a.Cmp(b); err != nil || c != 1 {
		t.Errorf("Cmp(a,b) = %d, %v", c, err)
	}
	if c, err := b.Cmp(a); err != nil || c != -1 {
		t.Errorf("Cmp(b,a) = %d, %v", c, err)
	}
	if c, err := a.Cmp(a); err != nil || c != 0 {
		t.Errorf("Cmp(a,a) = %d, %v", c, err)
	}
	zero, _ := Zero(BRL)
	if !zero.IsZero() || zero.String() != "0.00" {
		t.Errorf("Zero = %v", zero)
	}
	if !a.Equal(mustParse(t, "100.00", "BRL")) || a.Equal(b) {
		t.Error("Equal")
	}
}

func TestCurrencyMismatch(t *testing.T) {
	brl := mustParse(t, "1.00", "BRL")
	usd := mustParse(t, "1.00", "USD")
	if _, err := brl.Add(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Add = %v", err)
	}
	if _, err := brl.Sub(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Sub = %v", err)
	}
	if _, err := brl.Cmp(usd); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Cmp = %v", err)
	}
	if brl.Equal(usd) {
		t.Error("Equal across currencies")
	}
}

func TestOverflow(t *testing.T) {
	maxM := mustMinor(t, math.MaxInt64, BRL)
	minM := mustMinor(t, math.MinInt64, BRL)
	one := mustMinor(t, 1, BRL)
	minusOne := mustMinor(t, -1, BRL)

	cases := []struct {
		name string
		op   func() (Money, error)
	}{
		{"max + 1", func() (Money, error) { return maxM.Add(one) }},
		{"min + -1", func() (Money, error) { return minM.Add(minusOne) }},
		{"min - 1", func() (Money, error) { return minM.Sub(one) }},
		{"max - -1", func() (Money, error) { return maxM.Sub(minusOne) }},
		{"0 - min", func() (Money, error) { z, _ := Zero(BRL); return z.Sub(minM) }},
		{"-min", func() (Money, error) { return minM.Neg() }},
	}
	for _, c := range cases {
		if _, err := c.op(); !errors.Is(err, ErrOverflow) {
			t.Errorf("%s = %v, want ErrOverflow", c.name, err)
		}
	}

	// Edges that fit.
	if r, err := maxM.Add(minusOne); err != nil || r.Minor() != math.MaxInt64-1 {
		t.Errorf("max + -1 = %v, %v", r, err)
	}
	if r, err := minM.Sub(minusOne); err != nil || r.Minor() != math.MinInt64+1 {
		t.Errorf("min - -1 = %v, %v", r, err)
	}
	if r, err := maxM.Neg(); err != nil || r.Minor() != -math.MaxInt64 {
		t.Errorf("-max = %v, %v", r, err)
	}
}

func TestParseAmountOverflowCheck(t *testing.T) {
	// The integer-digit limit stops this before the checked arithmetic, but
	// the checked path itself must also refuse values beyond int64.
	if _, err := parseAmount("92233720368547758.08"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := parseAmountUnbounded("92233720368547758.08"); !errors.Is(err, ErrOverflow) {
		t.Fatalf("unbounded parse = %v, want ErrOverflow", err)
	}
	if v, err := parseAmountUnbounded("92233720368547758.07"); err != nil || v != math.MaxInt64 {
		t.Fatalf("unbounded parse of max = %d, %v", v, err)
	}
}

// parseAmountUnbounded runs the digit loop without the integer-digit limit,
// to exercise the overflow check directly.
func parseAmountUnbounded(s string) (int64, error) {
	var minor int64
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			continue
		}
		d := int64(s[i] - '0')
		if minor > (math.MaxInt64-d)/10 {
			return 0, ErrOverflow
		}
		minor = minor*10 + d
	}
	return minor, nil
}

func TestString(t *testing.T) {
	tests := []struct {
		minor int64
		want  string
	}{
		{0, "0.00"}, {5, "0.05"}, {-5, "-0.05"}, {-50, "-0.50"}, {123456, "1234.56"},
		{math.MaxInt64, "92233720368547758.07"},
		{math.MinInt64, "-92233720368547758.08"},
	}
	for _, tt := range tests {
		if got := mustMinor(t, tt.minor, BRL).String(); got != tt.want {
			t.Errorf("String(%d) = %q, want %q", tt.minor, got, tt.want)
		}
	}
}

func TestJSON(t *testing.T) {
	m := mustParse(t, "25.00", "BRL")
	b, err := json.Marshal(m)
	if err != nil || string(b) != `{"amount":"25.00","currency":"BRL"}` {
		t.Fatalf("Marshal = %s, %v", b, err)
	}
	var back Money
	if err := json.Unmarshal(b, &back); err != nil || !back.Equal(m) {
		t.Fatalf("round trip = %v, %v", back, err)
	}

	neg := mustMinor(t, -500, BRL)
	if b, err := json.Marshal(neg); err != nil || string(b) != `{"amount":"-5.00","currency":"BRL"}` {
		t.Fatalf("Marshal negative = %s, %v", b, err)
	}

	rejects := []string{
		`{"amount":25.00,"currency":"BRL"}`,
		`{"amount":25,"currency":"BRL"}`,
		`{"amount":"25","currency":"BRL"}`,
		`{"amount":"-5.00","currency":"BRL"}`,
		`{"amount":"1e3","currency":"BRL"}`,
		`{"amount":"NaN","currency":"BRL"}`,
		`{"amount":null,"currency":"BRL"}`,
		`{"currency":"BRL"}`,
		`{"amount":"25.00"}`,
		`{"amount":"25.00","currency":"brl"}`,
		`{"amount":"25.00","currency":1}`,
		`"25.00"`,
		`null`,
	}
	for _, in := range rejects {
		var got Money
		if err := json.Unmarshal([]byte(in), &got); err == nil {
			t.Errorf("Unmarshal(%s) succeeded with %v", in, got)
		}
	}
	var unsupported Money
	if err := json.Unmarshal([]byte(`{"amount":"1.00","currency":"EUR"}`), &unsupported); !errors.Is(err, ErrUnsupportedCurrency) {
		t.Errorf("Unmarshal EUR = %v", err)
	}
}

func FuzzParseRoundTrip(f *testing.F) {
	for _, s := range []string{"0.00", "25.00", "999999999999.99", "1.5", "-1.00", "1e3", "NaN", "01.00", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		m, err := Parse(s, "BRL")
		if err != nil {
			return
		}
		if m.String() != s {
			t.Fatalf("accepted %q but String() = %q", s, m.String())
		}
		if m.IsNegative() || m.Minor() > MaxExternalMinor {
			t.Fatalf("accepted out-of-range %q -> %d", s, m.Minor())
		}
	})
}
