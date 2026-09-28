package auctionclearing

import (
	"encoding/json"
	"errors"
	"math/big"
	"strings"
)

// moneyScale 是 Money 的固定小数精度（纳级，1 单位 = 10^9 最小单位）。
// 金额一律使用该定点整数精确表示，不经过 float，避免二进制浮点误差。
const moneyScale = 9

var moneyScaleBig = big.NewInt(1e9)

// Money 是不可变的定点金额：内部以 10^-9 为单位的 *big.Int 表示。
// 零值即 0 金额；比较、加减、乘数量均为精确运算。
type Money struct {
	v *big.Int
}

// ParseMoney 解析十进制金额字符串，如 "1.50"、"10"、"0.001"。
// 超过 moneyScale 位小数会被拒绝（宁可不精确，也不静默丢精度）。
func ParseMoney(s string) (Money, error) {
	orig := s
	neg := false
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" || strings.ContainsAny(s, "eE") {
		return Money{}, errors.New("invalid money: " + orig)
	}
	intPart, fracPart, ok := strings.Cut(s, ".")
	if !ok {
		intPart = s
	}
	if intPart == "" {
		intPart = "0"
	}
	if !isAllDigits(intPart) {
		return Money{}, errors.New("invalid money: " + orig)
	}
	if ok {
		if fracPart == "" || !isAllDigits(fracPart) {
			return Money{}, errors.New("invalid money: " + orig)
		}
		if len(fracPart) > moneyScale {
			return Money{}, errors.New("money precision exceeds " + itoa(moneyScale) + " decimals: " + orig)
		}
		fracPart += strings.Repeat("0", moneyScale-len(fracPart))
	} else {
		fracPart = strings.Repeat("0", moneyScale)
	}
	raw, ok := new(big.Int).SetString(intPart+fracPart, 10)
	if !ok {
		return Money{}, errors.New("invalid money: " + orig)
	}
	if neg {
		raw.Neg(raw)
	}
	return Money{v: raw}, nil
}

// MustParseMoney 在解析失败时 panic，主要用于测试与常量构造。
func MustParseMoney(s string) Money {
	m, err := ParseMoney(s)
	if err != nil {
		panic(err)
	}
	return m
}

// ZeroMoney 返回 0 金额。
func ZeroMoney() Money { return Money{v: new(big.Int)} }

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func (m Money) raw() *big.Int {
	if m.v == nil {
		return new(big.Int)
	}
	return m.v
}

// String 以十进制字符串输出，去掉小数尾零，保证 ParseMoney(String()) 幂等。
func (m Money) String() string {
	raw := m.raw()
	neg := raw.Sign() < 0
	if neg {
		raw = new(big.Int).Neg(raw)
	}
	q, r := new(big.Int).QuoRem(raw, moneyScaleBig, new(big.Int))
	whole := q.String()
	frac := r.String()
	if len(frac) < moneyScale {
		frac = strings.Repeat("0", moneyScale-len(frac)) + frac
	}
	frac = strings.TrimRight(frac, "0")
	out := whole
	if frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

// IsZero 报告金额是否为 0。
func (m Money) IsZero() bool { return m.raw().Sign() == 0 }

// Positive 报告金额是否严格大于 0。
func (m Money) Positive() bool { return m.raw().Sign() > 0 }

// Cmp 比较金额：m<other 返回 -1，相等 0，否则 1。
func (m Money) Cmp(other Money) int { return m.raw().Cmp(other.raw()) }

// MulQuantity 返回 单价 * 整数数量，结果仍是精确金额。
func (m Money) MulQuantity(qty int64) Money {
	return Money{v: new(big.Int).Mul(m.raw(), big.NewInt(qty))}
}

// Add 返回 m+other。
func (m Money) Add(other Money) Money {
	return Money{v: new(big.Int).Add(m.raw(), other.raw())}
}

// MarshalJSON 把金额序列化为 JSON 字符串，杜绝浮点中转。
func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}

// UnmarshalJSON 仅接受 JSON 字符串形式的十进制金额。
func (m *Money) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return errors.New("money must be a JSON string")
	}
	parsed, err := ParseMoney(s)
	if err != nil {
		return err
	}
	m.v = parsed.v
	return nil
}
