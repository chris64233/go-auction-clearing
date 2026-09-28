package auctionclearing

import (
	"encoding/json"
	"testing"
)

func TestParseMoney(t *testing.T) {
	cases := []struct {
		in   string
		want Money
	}{
		{"0", 0},
		{"10", MustParseMoney("10")},
		{"10.5", 105000},
		{"10.5000", 105000},
		{"0.0001", 1},
		{".5", 5000},
		{"-1.25", -12500},
		{"+2", 20000},
	}
	for _, c := range cases {
		got, err := ParseMoney(c.in)
		if err != nil {
			t.Fatalf("ParseMoney(%q) error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseMoney(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseMoneyInvalid(t *testing.T) {
	for _, in := range []string{"", "abc", "1.2.3", "1.00001", "-", "1e3", "99999999999999999999999"} {
		if _, err := ParseMoney(in); err == nil {
			t.Errorf("ParseMoney(%q) expected error", in)
		}
	}
}

func TestMoneyString(t *testing.T) {
	cases := map[string]string{
		"0":       "0",
		"10.5000": "10.5",
		"0.0001":  "0.0001",
		"-1.2500": "-1.25",
		"100":     "100",
		"3.1000":  "3.1",
	}
	for in, want := range cases {
		m, err := ParseMoney(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := m.String(); got != want {
			t.Errorf("%q.String() = %q, want %q", in, got, want)
		}
	}
}

func TestMoneyParseRoundTrip(t *testing.T) {
	for _, s := range []string{"0", "12.3456", "9999999.9999", "-0.0001"} {
		m, err := ParseMoney(s)
		if err != nil {
			t.Fatal(err)
		}
		back, err := ParseMoney(m.String())
		if err != nil {
			t.Fatal(err)
		}
		if back != m {
			t.Errorf("round trip %s -> %s -> %d", s, m.String(), back)
		}
	}
}

func TestMoneyMulAndAdd(t *testing.T) {
	total, err := MustParseMoney("9.5").Mul(10)
	if err != nil {
		t.Fatal(err)
	}
	if total.String() != "95" {
		t.Errorf("9.5 * 10 = %s, want 95", total)
	}
	sum, err := MustParseMoney("1.1").Add(MustParseMoney("2.2"))
	if err != nil || sum.String() != "3.3" {
		t.Errorf("1.1 + 2.2 = %s, err=%v", sum, err)
	}
}

func TestMoneyJSON(t *testing.T) {
	type wrap struct {
		Price Money `json:"price"`
	}
	raw, err := json.Marshal(wrap{Price: MustParseMoney("10.5")})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"price":"10.5"}` {
		t.Fatalf("marshal = %s", raw)
	}
	var w wrap
	if err := json.Unmarshal([]byte(`{"price":"10.5000"}`), &w); err != nil {
		t.Fatal(err)
	}
	if w.Price != MustParseMoney("10.5") {
		t.Fatalf("unmarshal = %d", w.Price)
	}
	// JSON number 必须被拒绝，防止调用方以浮点传金额。
	if err := json.Unmarshal([]byte(`{"price":10.5}`), &w); err == nil {
		t.Fatal("numeric JSON amount should be rejected")
	}
}
