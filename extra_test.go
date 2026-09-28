package auctionclearing

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestListBids(t *testing.T) {
	e, clk, _ := newTestEngine(t)
	mustCreateAuction(t, e, 10, clk.Now().Add(time.Hour), "0")
	in := func(ext string) BidInput {
		return BidInput{AuctionID: 1, ExternalID: ext, Bidder: "A", Qty: 1, Price: MustParseMoney("1")}
	}
	_, _ = e.SubmitBid(in("z"))
	_, _ = e.SubmitBid(in("a"))
	_, _ = e.SubmitBid(in("m"))
	if _, err := e.WithdrawBid(1, "a"); err != nil {
		t.Fatal(err)
	}

	bids, err := e.ListBids(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(bids) != 3 || bids[0].ExternalID != "a" || bids[1].ExternalID != "m" || bids[2].ExternalID != "z" {
		t.Fatalf("bids order = %+v", bids)
	}
	if bids[0].Status != BidWithdrawn {
		t.Errorf("withdrawn status lost in listing: %+v", bids[0])
	}
	if _, err := e.ListBids(999); !errors.Is(err, ErrAuctionNotFound) {
		t.Errorf("missing auction: err=%v", err)
	}
}

func TestControlledClockAdvance(t *testing.T) {
	clk := NewControlledClock(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	start := clk.Now()
	clk.Advance(5 * time.Second)
	if !clk.Now().Equal(start.Add(5 * time.Second)) {
		t.Fatalf("advance: %s", clk.Now().Sub(start))
	}
	clk.Set(start)
	if !clk.Now().Equal(start) {
		t.Fatalf("set failed")
	}
}

func TestMoneyOverflow(t *testing.T) {
	big := Money(1 << 62)
	if _, err := big.Add(big); err == nil {
		t.Error("add overflow expected")
	}
	if _, err := Money(4).Mul(1 << 62); err == nil {
		t.Error("mul overflow expected")
	}
	if _, err := Money(1).Mul(0); err != nil {
		t.Error("mul by zero should succeed")
	}
}

// failingStore 在满足条件时让 Append 失败，用于验证清算的失败原子性。
type failingStore struct {
	inner      EventStore
	failOnceOn EventType
	failed     bool
}

func (s *failingStore) Append(events []Event) ([]Event, error) {
	for _, ev := range events {
		if !s.failed && ev.Type == s.failOnceOn {
			s.failed = true
			return nil, fmt.Errorf("injected failure on %s", ev.Type)
		}
	}
	return s.inner.Append(events)
}

func (s *failingStore) Load() ([]Event, error) { return s.inner.Load() }

// 清算落盘失败：不遗留成交记录，拍卖回到开放态，重试可以成功。
func TestSettleFailureLeavesNoPartialState(t *testing.T) {
	clk := NewControlledClock(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	store := &failingStore{inner: NewMemoryEventStore(), failOnceOn: EvSettled}
	e, err := NewEngine(store, clk)
	if err != nil {
		t.Fatal(err)
	}
	dl := clk.Now().Add(time.Hour)
	mustCreateAuction(t, e, 5, dl, "0")
	_, _ = e.SubmitBid(BidInput{AuctionID: 1, ExternalID: "b1", Bidder: "A", Qty: 5, Price: MustParseMoney("10")})
	clk.Set(dl)

	if _, err := e.SettleAuction(1); err == nil {
		t.Fatal("expected injected settlement failure")
	}
	// 失败后：没有成交结果、拍卖仍 open、报价仍有效。
	if _, err := e.GetSettlement(1); !errors.Is(err, ErrSettlementNotFound) {
		t.Fatalf("settlement exists after failure: %v", err)
	}
	a, _ := e.GetAuction(1)
	if a.Status != AuctionOpen {
		t.Fatalf("auction status = %s, want open", a.Status)
	}
	b, _ := e.GetBid(1, "b1")
	if b.Status != BidActive {
		t.Fatalf("bid status = %s, want active", b.Status)
	}
	// 事件日志中不应有 settled 事件。
	events, _ := store.Load()
	for _, ev := range events {
		if ev.Type == EvSettled {
			t.Fatal("settled event persisted despite failure")
		}
	}
	// 重试：清算成功，且只成功一次。
	r, err := e.SettleAuction(1)
	if err != nil || r.Outcome != SettleSettled || r.Data.SoldQty != 5 {
		t.Fatalf("retry settle: %+v err=%v", r, err)
	}
}
