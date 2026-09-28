package auctionclearing

import (
	"encoding/json"
	"testing"
)

func TestParseMoneyAndString(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"1", "1"},
		{"1.50", "1.5"},
		{"0.001", "0.001"},
		{"100.123456789", "100.123456789"},
		{"0", "0"},
		{"0.000000001", "0.000000001"},
		{"-2.5", "-2.5"},
	}
	for _, c := range cases {
		m, err := ParseMoney(c.in)
		if err != nil {
			t.Fatalf("ParseMoney(%q) error: %v", c.in, err)
		}
		if got := m.String(); got != c.want {
			t.Errorf("ParseMoney(%q).String() = %q, want %q", c.in, got, c.want)
		}
		// 字符串表示必须可再次精确解析且相等。
		if rt, err := ParseMoney(m.String()); err != nil || rt.Cmp(m) != 0 {
			t.Errorf("roundtrip failed for %q", c.in)
		}
	}
}

func TestParseMoneyRejects(t *testing.T) {
	bad := []string{"", "abc", "1.", ".", "1.2.3", "1e3", "0.0000000001", "1,000"}
	for _, s := range bad {
		if _, err := ParseMoney(s); err == nil {
			t.Errorf("ParseMoney(%q) expected error", s)
		}
	}
}

func TestMoneyArithmetic(t *testing.T) {
	// 0.1 + 0.2 必须精确等于 0.3（浮点会出错的经典场景）。
	a := MustParseMoney("0.1")
	b := MustParseMoney("0.2")
	if got := a.Add(b).String(); got != "0.3" {
		t.Errorf("0.1+0.2 = %s, want 0.3", got)
	}
	// 单价 1.99 * 数量 3 = 5.97，精确。
	if got := MustParseMoney("1.99").MulQuantity(3).String(); got != "5.97" {
		t.Errorf("1.99*3 = %s, want 5.97", got)
	}
	// 负数数量乘法。
	if got := MustParseMoney("2").MulQuantity(-3).String(); got != "-6" {
		t.Errorf("2*-3 = %s, want -6", got)
	}
	if MustParseMoney("1").Cmp(MustParseMoney("1.0")) != 0 {
		t.Error("1 != 1.0")
	}
	if !MustParseMoney("0.01").Positive() || ZeroMoney().Positive() {
		t.Error("Positive() wrong")
	}
}

func TestMoneyJSON(t *testing.T) {
	type box struct{ Price Money }
	orig := box{Price: MustParseMoney("12.345")}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"Price":"12.345"}` {
		t.Errorf("marshal = %s", data)
	}
	var back box
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Price.Cmp(orig.Price) != 0 {
		t.Errorf("json roundtrip mismatch: %s vs %s", back.Price, orig.Price)
	}
	// 拒绝 JSON 数字，强制金额走字符串。
	if err := json.Unmarshal([]byte(`{"Price":1.5}`), &back); err == nil {
		t.Error("expected error unmarshaling number into Money")
	}
}
