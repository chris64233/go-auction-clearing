package auctionclearing

import (
	"testing"
	"time"
)

func bidForTest(ext, bidder string, qty int64, price string, submitted time.Time, status BidStatus) Bid {
	return Bid{
		AuctionID:   1,
		ExternalID:  ext,
		Bidder:      bidder,
		Qty:         qty,
		Price:       MustParseMoney(price),
		SubmittedAt: submitted,
		UpdatedAt:   submitted,
		Status:      status,
	}
}

// 价格降序 + 边界报价部分成交 + 统一清算价。
func TestSettlePriceDescendingWithPartialFill(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	bids := []Bid{
		bidForTest("b3", "C", 4, "8", t0.Add(3*time.Second), BidActive),
		bidForTest("b1", "A", 3, "10", t0, BidActive),
		bidForTest("b2", "B", 5, "9", t0.Add(1*time.Second), BidActive),
	}
	// 可售 10：b1 全成 3，b2 全成 5，b3 部分成 2。
	st, err := settleFromSnapshot(10, 0, bids, t0.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if st.SoldQty != 10 {
		t.Fatalf("SoldQty = %d, want 10", st.SoldQty)
	}
	if st.ClearingPrice.String() != "8" {
		t.Fatalf("ClearingPrice = %s, want 8", st.ClearingPrice)
	}
	if len(st.Fills) != 3 {
		t.Fatalf("fills = %d, want 3", len(st.Fills))
	}
	want := []struct {
		ext    string
		filled int64
		price  string
		rank   int
	}{
		{"b1", 3, "8", 1},
		{"b2", 5, "8", 2},
		{"b3", 2, "8", 3},
	}
	for i, w := range want {
		got := st.Fills[i]
		if got.ExternalID != w.ext || got.FilledQty != w.filled || got.Price.String() != w.price || got.Rank != w.rank {
			t.Errorf("fill[%d] = %+v, want ext=%s filled=%d price=%s rank=%d", i, got, w.ext, w.filled, w.price, w.rank)
		}
	}
	// 统一价 8 * 10。
	if st.TotalProceeds.String() != "80" {
		t.Errorf("TotalProceeds = %s, want 80", st.TotalProceeds)
	}
	// 输入快照不得被修改。
	if bids[0].Status != BidActive {
		t.Error("snapshot was mutated")
	}
}

// 同价按首次有效提交先后排序。
func TestSettleSamePriceTieBreakBySubmissionTime(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	bids := []Bid{
		bidForTest("late", "L", 4, "7", t0.Add(2*time.Second), BidActive),
		bidForTest("early", "E", 4, "7", t0, BidActive),
		bidForTest("mid", "M", 4, "7", t0.Add(1*time.Second), BidActive),
	}
	// 可售 10：early 4、mid 4、late 2。
	st, err := settleFromSnapshot(10, 0, bids, t0)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{st.Fills[0].ExternalID, st.Fills[1].ExternalID, st.Fills[2].ExternalID}
	want := []string{"early", "mid", "late"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if st.Fills[2].FilledQty != 2 {
		t.Errorf("boundary fill = %d, want 2", st.Fills[2].FilledQty)
	}
}

// 最低成交价过滤。
func TestSettleReservePrice(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	bids := []Bid{
		bidForTest("ok2", "B", 3, "10", t0.Add(1*time.Second), BidActive),
		bidForTest("ok1", "A", 3, "10", t0, BidActive),
		bidForTest("below", "X", 100, "9.9999", t0, BidActive),
	}
	st, err := settleFromSnapshot(10, MustParseMoney("10"), bids, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Fills) != 2 || st.SoldQty != 6 {
		t.Fatalf("fills=%d sold=%d, want 2 fills / 6 sold", len(st.Fills), st.SoldQty)
	}
	for _, f := range st.Fills {
		if f.ExternalID == "below" {
			t.Error("bid below reserve was filled")
		}
	}
}

// 已撤回的报价不能进入快照分配。
func TestSettleExcludesWithdrawn(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	bids := []Bid{
		bidForTest("a1", "A", 5, "12", t0, BidWithdrawn),
		bidForTest("a2", "B", 5, "11", t0.Add(1*time.Second), BidActive),
	}
	st, err := settleFromSnapshot(10, 0, bids, t0)
	if err != nil {
		t.Fatal(err)
	}
	if st.SoldQty != 5 || len(st.Fills) != 1 || st.Fills[0].ExternalID != "a2" {
		t.Fatalf("settlement = %+v, want only a2 filled 5", st)
	}
}

// 报价总量不足：可售量用不完，边界为最后一份全成报价。
func TestSettleSupplyNotExhausted(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	bids := []Bid{
		bidForTest("a1", "A", 2, "12", t0, BidActive),
		bidForTest("a2", "B", 3, "11", t0, BidActive),
	}
	st, err := settleFromSnapshot(100, 0, bids, t0)
	if err != nil {
		t.Fatal(err)
	}
	if st.SoldQty != 5 || st.ClearingPrice.String() != "11" || st.TotalProceeds.String() != "55" {
		t.Fatalf("settlement = %+v", st)
	}
}

// 恰好售罄：最后一份全成，不存在部分成交。
func TestSettleExactExhaustion(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	bids := []Bid{
		bidForTest("a1", "A", 4, "12", t0, BidActive),
		bidForTest("a2", "B", 6, "11", t0, BidActive),
		bidForTest("a3", "C", 1, "10", t0.Add(1*time.Second), BidActive),
	}
	st, err := settleFromSnapshot(10, 0, bids, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Fills) != 2 {
		t.Fatalf("fills = %d, want 2", len(st.Fills))
	}
	if st.Fills[1].ExternalID != "a2" || st.Fills[1].FilledQty != 6 {
		t.Fatalf("last fill = %+v, want a2 fully filled 6", st.Fills[1])
	}
}

// 无有效报价 / 全部低于保留价。
func TestSettleNoFills(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	st, err := settleFromSnapshot(5, MustParseMoney("100"), []Bid{
		bidForTest("a1", "A", 1, "1", t0, BidActive),
		bidForTest("a2", "B", 1, "2", t0, BidWithdrawn),
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if st.SoldQty != 0 || st.ClearingPrice != 0 || len(st.Fills) != 0 || st.TotalProceeds != 0 {
		t.Fatalf("settlement = %+v, want empty", st)
	}
}
