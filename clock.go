package auctionclearing

import (
	"sync"
	"time"
)

// Clock 是统一的当前时间来源。
// 报价截止判断、报价提交时间、撤回时间、清算时间全部经由 Clock 获取，
// 生产环境使用 SystemClock，测试使用 ControlledClock 精确控制边界。
type Clock interface {
	Now() time.Time
}

// SystemClock 使用操作系统墙上时间。
type SystemClock struct{}

// Now 返回 time.Now()。
func (SystemClock) Now() time.Time { return time.Now() }

// ControlledClock 是可手动设置/推进的时钟，供测试使用，并发安全。
type ControlledClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewControlledClock 以指定时间构造可控时钟。
func NewControlledClock(t time.Time) *ControlledClock {
	return &ControlledClock{now: t}
}

// Now 返回当前受控时间。
func (c *ControlledClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Set 将时钟直接设置到指定时刻。
func (c *ControlledClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// Advance 将时钟向前推进 d。
func (c *ControlledClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
