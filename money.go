package auctionclearing

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
)

// MoneyScale 是金额的固定小数精度：1 个货币单位等于 10^MoneyScale 个最小记账单位。
// 所有金额一律使用 int64 定点数表示，避免浮点运算带来的精度误差。
const MoneyScale = 4

// Money 是精确金额，底层为 int64 定点数，单位为 10^-MoneyScale 个货币单位。
type Money int64

// ErrInvalidMoney 表示金额字符串无法解析为精确金额。
var ErrInvalidMoney = errors.New("invalid money value")

// ParseMoney 将十进制金额字符串解析为 Money，小数位不得超过 MoneyScale 位。
// 例如 "10.5" 与 "10.5000" 表示同一金额；超过 int64 范围或精度非法时返回错误。
func ParseMoney(s string) (Money, error) {
	if s == "" {
		return 0, ErrInvalidMoney
	}
	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}
	if s == "" {
		return 0, ErrInvalidMoney
	}
	whole, frac := s, ""
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		whole, frac = s[:dot], s[dot+1:]
		if whole == "" {
			whole = "0"
		}
	}
	if whole == "" || len(frac) > MoneyScale {
		return 0, ErrInvalidMoney
	}
	for _, r := range whole {
		if r < '0' || r > '9' {
			return 0, ErrInvalidMoney
		}
	}
	for _, r := range frac {
		if r < '0' || r > '9' {
			return 0, ErrInvalidMoney
		}
	}
	frac = frac + strings.Repeat("0", MoneyScale-len(frac))
	v, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		// 主要是溢出。
		return 0, ErrInvalidMoney
	}
	if neg {
		v = -v
	}
	return Money(v), nil
}

// MustParseMoney 在解析失败时 panic，适合测试与常量构造。
func MustParseMoney(s string) Money {
	m, err := ParseMoney(s)
	if err != nil {
		panic(err)
	}
	return m
}

// String 返回规范十进制字符串：至少 1 位整数，小数部分去掉尾随零。
func (m Money) String() string {
	neg := m < 0
	v := int64(m)
	if neg {
		v = -v
	}
	whole := v / pow10(MoneyScale)
	frac := v % pow10(MoneyScale)
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(strconv.FormatInt(whole, 10))
	if frac > 0 {
		fs := strconv.FormatInt(frac, 10)
		fs = strings.Repeat("0", MoneyScale-len(fs)) + fs
		fs = strings.TrimRight(fs, "0")
		b.WriteByte('.')
		b.WriteString(fs)
	}
	return b.String()
}

// IsNegative 判断金额是否为负。
func (m Money) IsNegative() bool { return m < 0 }

// Cmp 比较两个金额：m > o 返回 1，相等返回 0，m < o 返回 -1。
func (m Money) Cmp(o Money) int {
	switch {
	case m > o:
		return 1
	case m < o:
		return -1
	default:
		return 0
	}
}

// Add 精确加法，溢出时返回错误。
func (m Money) Add(o Money) (Money, error) {
	r := int64(m) + int64(o)
	if (o > 0 && r < int64(m)) || (o < 0 && r > int64(m)) {
		return 0, errors.New("money overflow")
	}
	return Money(r), nil
}

// Mul 将单价乘以整数数量，溢出时返回错误（成交金额计算使用）。
func (m Money) Mul(qty int64) (Money, error) {
	if m == 0 || qty == 0 {
		return 0, nil
	}
	if qty == math.MinInt64 {
		return 0, errors.New("money overflow")
	}
	r := int64(m) * qty
	if r/qty != int64(m) {
		return 0, errors.New("money overflow")
	}
	return Money(r), nil
}

func pow10(n int) int64 {
	v := int64(1)
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}

// MarshalJSON 以十进制字符串输出金额，接口边界上杜绝浮点精度损失。
func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}

// UnmarshalJSON 仅接受十进制字符串（如 "10.50"），拒绝 JSON number。
func (m *Money) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return ErrInvalidMoney
	}
	v, err := ParseMoney(s)
	if err != nil {
		return err
	}
	*m = v
	return nil
}
