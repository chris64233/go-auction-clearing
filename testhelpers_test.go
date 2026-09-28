package auctionclearing

import (
	"time"
)

// fakeClock 是测试用可控时钟，统一驱动截止边界判断。
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }
func (c *fakeClock) Set(t time.Time)         { c.t = t }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)}
}

// buildAggregate 构造一个测试聚合。
func buildAggregate(id string, qty int64, reserve string, deadline time.Time, bids ...Bid) *Aggregate {
	agg := &Aggregate{
		Auction: Auction{
			ID: id, AvailableQty: qty, Deadline: deadline,
			ReservePrice: MustParseMoney(reserve), Status: StatusOpen,
		},
		Bids: map[string]*Bid{},
	}
	for i := range bids {
		b := bids[i]
		b.AuctionID = id
		if b.FirstSubmittedAt.IsZero() {
			b.FirstSubmittedAt = deadline.Add(-time.Duration(len(bids)-i) * time.Minute)
		}
		b.UpdatedAt = b.FirstSubmittedAt
		b.State = BidActive
		agg.Bids[b.ExternalRef] = &b
	}
	return agg
}

func activeBid(ref, bidder string, qty int64, price string, submitted time.Time) Bid {
	return Bid{ExternalRef: ref, Bidder: bidder, Quantity: qty,
		UnitPrice: MustParseMoney(price), FirstSubmittedAt: submitted, State: BidActive}
}
