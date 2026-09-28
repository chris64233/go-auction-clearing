package auctionclearing

import (
	"context"
	"testing"
	"time"
)

func TestFileRepository_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()

	repo, err := NewFileRepository(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(repo, clock)
	a, err := svc.CreateAuction(context.Background(), CreateAuctionParams{
		AvailableQty: 20,
		Deadline:     clock.Now().Add(time.Hour),
		ReservePrice: MustParseMoney("5.5"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PlaceBid(context.Background(), a.ID, bidParams("ORD-1", 12, "9.99")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PlaceBid(context.Background(), a.ID, bidParams("ORD-2", 3, "7")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.WithdrawBid(context.Background(), a.ID, "ORD-2"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Hour)
	out, err := svc.ClearAuction(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Result.SoldQty != 12 {
		t.Fatalf("sold = %d, want 12", out.Result.SoldQty)
	}

	// 重新从磁盘加载：拍卖、报价（含撤回状态）与清算结果都必须完整恢复。
	reloaded, err := LoadFileRepository(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc2 := NewService(reloaded, clock)
	got, err := svc2.GetAuction(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCleared || got.AvailableQty != 20 {
		t.Fatalf("reloaded auction = %+v", got)
	}
	b, err := svc2.GetBid(context.Background(), a.ID, "ORD-2")
	if err != nil || b.State != BidWithdrawn {
		t.Fatalf("reloaded withdrawn bid: %+v %v", b, err)
	}
	res, err := svc2.GetResult(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.SoldQty != 12 || len(res.Fills) != 1 || res.Fills[0].UnitPrice.String() != "9.99" {
		t.Fatalf("reloaded result mismatch: %+v", res)
	}
	if got := res.TotalProceeds.String(); got != "119.88" {
		t.Fatalf("proceeds = %s, want 119.88 (12 x 9.99)", got)
	}
	// 重新加载后再次清算必须读到原结果，而不是二次成交。
	replay, err := svc2.ClearAuction(context.Background(), a.ID)
	if err != nil || !replay.Replayed || !sameResult(replay.Result, res) {
		t.Fatalf("post-reload clear must replay stored result: %+v %v", replay, err)
	}
}

func TestFileRepository_FailedTransactionLeavesNoTrace(t *testing.T) {
	dir := t.TempDir()
	repo, err := NewFileRepository(dir)
	if err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	svc := NewService(repo, clock)
	a, err := svc.CreateAuction(context.Background(), CreateAuctionParams{
		AvailableQty: 5, Deadline: clock.Now().Add(time.Hour), ReservePrice: MustParseMoney("1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 一个返回错误的事务必须整体放弃：不得新增报价、不得改状态、磁盘文件不变。
	_, err = repo.Update(context.Background(), a.ID, func(agg *Aggregate) error {
		agg.Bids["ghost"] = &Bid{ExternalRef: "ghost", State: BidActive}
		agg.Auction.Status = StatusCleared
		return NewError(KindConflict, "boom")
	})
	if ErrorKindOf(err) != KindConflict {
		t.Fatalf("err = %v", err)
	}
	agg, err := repo.Load(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := agg.Bids["ghost"]; exists {
		t.Fatal("rolled-back transaction left a partial write")
	}
	if agg.Auction.Status != StatusOpen {
		t.Fatal("rolled-back transaction flipped status")
	}
	if agg.Clearing != nil {
		t.Fatal("rolled-back transaction left fills")
	}
}
