package auctionclearing

import (
	"testing"
	"time"
)

func TestClear_PriceDescendingAndTieBreakFIFO(t *testing.T) {
	deadline := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	t1 := deadline.Add(-10 * time.Minute)
	t2 := deadline.Add(-5 * time.Minute)
	t3 := deadline.Add(-1 * time.Minute)

	// b1/b2 同价 10，b1 先提交；b3 价高。可售量足够全部成交。
	agg := buildAggregate("a1", 100, "1", deadline,
		activeBid("b2", "B", 20, "10", t2),
		activeBid("b3", "C", 10, "20", t3), // 晚提交但价最高
		activeBid("b1", "A", 30, "10", t1),
	)
	snap := takeSnapshot(agg, deadline)
	res := clearFromSnapshot(agg.Auction, snap)

	if res.SoldQty != 60 {
		t.Fatalf("sold = %d, want 60", res.SoldQty)
	}
	wantSeq := []string{"b3", "b1", "b2"}
	if len(res.Fills) != 3 {
		t.Fatalf("fills = %d, want 3", len(res.Fills))
	}
	for i, ref := range wantSeq {
		if res.Fills[i].ExternalRef != ref {
			t.Errorf("fill[%d] = %s, want %s (price desc, same-price FIFO)", i, res.Fills[i].ExternalRef, ref)
		}
	}
	if got := res.TotalProceeds.String(); got != "700" {
		t.Errorf("proceeds = %s, want 700 (b3:200 + b1:300 + b2:200)", got)
	}
}

func TestClear_PartialFillAtBoundary(t *testing.T) {
	deadline := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	base := deadline.Add(-10 * time.Minute)
	agg := buildAggregate("a1", 25, "1", deadline,
		activeBid("high", "H", 10, "20", base),
		activeBid("mid", "M", 20, "10", base.Add(time.Minute)), // 边界报价，只能拿到 15
	)
	res := clearFromSnapshot(agg.Auction, takeSnapshot(agg, deadline))

	if res.SoldQty != 25 {
		t.Fatalf("sold = %d, want 25", res.SoldQty)
	}
	if len(res.Fills) != 2 {
		t.Fatalf("fills = %d, want 2", len(res.Fills))
	}
	last := res.Fills[1]
	if last.ExternalRef != "mid" || last.Quantity != 15 {
		t.Fatalf("boundary fill = %+v, want mid qty 15", last)
	}
	if got := last.Amount.String(); got != "150" {
		t.Errorf("boundary amount = %s, want 150 (15 x 10)", got)
	}
	if got := res.TotalProceeds.String(); got != "350" {
		t.Errorf("proceeds = %s, want 350", got)
	}
}

func TestClear_ReserveAndWithdrawnExcluded(t *testing.T) {
	deadline := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	base := deadline.Add(-10 * time.Minute)
	agg := buildAggregate("a1", 100, "10", deadline,
		activeBid("at", "A", 5, "10", base),      // 恰好等于保留价：入选
		activeBid("below", "B", 5, "9.99", base), // 低于保留价：排除
		activeBid("gone", "C", 50, "100", base.Add(time.Minute)),
	)
	agg.Bids["gone"].State = BidWithdrawn // 已撤回：不得进入快照，更不能被恢复

	snap := takeSnapshot(agg, deadline)
	if len(snap.bids) != 1 || snap.bids[0].ExternalRef != "at" {
		refs := make([]string, 0, len(snap.bids))
		for _, b := range snap.bids {
			refs = append(refs, b.ExternalRef)
		}
		t.Fatalf("snapshot bids = %v, want [at]", refs)
	}
	res := clearFromSnapshot(agg.Auction, snap)
	if res.BidCount != 1 || res.SoldQty != 5 {
		t.Fatalf("result bidcount=%d sold=%d, want 1/5", res.BidCount, res.SoldQty)
	}
}

func TestClear_NeverExceedsAvailable(t *testing.T) {
	deadline := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	agg := buildAggregate("a1", 3, "1", deadline,
		activeBid("x", "X", 100, "10", deadline.Add(-2*time.Minute)),
		activeBid("y", "Y", 100, "10", deadline.Add(-1*time.Minute)),
	)
	res := clearFromSnapshot(agg.Auction, takeSnapshot(agg, deadline))
	if res.SoldQty != 3 {
		t.Fatalf("sold = %d, want exactly 3", res.SoldQty)
	}
	if res.Fills[0].Quantity != 3 { // 全部落在先提交的 x 上且部分成交
		t.Fatalf("first fill qty = %d, want 3", res.Fills[0].Quantity)
	}
	if len(res.Fills) != 1 {
		t.Fatalf("y should not be filled, got %d fills", len(res.Fills))
	}
}

func TestClear_NoBidsOrAllBelowReserve(t *testing.T) {
	deadline := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	agg := buildAggregate("a1", 10, "100", deadline,
		activeBid("low", "L", 5, "10", deadline.Add(-time.Minute)),
	)
	res := clearFromSnapshot(agg.Auction, takeSnapshot(agg, deadline))
	if res.SoldQty != 0 || len(res.Fills) != 0 || !res.TotalProceeds.IsZero() {
		t.Fatalf("expected empty result, got %+v", res)
	}
}

func TestClear_DoesNotMutateBids(t *testing.T) {
	deadline := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	agg := buildAggregate("a1", 5, "1", deadline,
		activeBid("x", "X", 10, "10", deadline.Add(-time.Minute)),
	)
	_ = clearFromSnapshot(agg.Auction, takeSnapshot(agg, deadline))
	if agg.Bids["x"].Quantity != 10 || agg.Bids["x"].State != BidActive {
		t.Fatal("clearing mutated source bid; snapshot must stay immutable")
	}
	if agg.Auction.Status != StatusOpen {
		t.Fatal("pure clearing must not flip auction status")
	}
}
