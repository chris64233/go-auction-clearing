package auctionclearing

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTestEngine(t *testing.T) (*Engine, *ControlledClock, *MemoryEventStore) {
	t.Helper()
	clk := NewControlledClock(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	store := NewMemoryEventStore()
	e, err := NewEngine(store, clk)
	if err != nil {
		t.Fatal(err)
	}
	return e, clk, store
}

func mustCreateAuction(t *testing.T, e *Engine, qty int64, deadline time.Time, reserve string) Auction {
	t.Helper()
	a, err := e.CreateAuction(qty, deadline, MustParseMoney(reserve))
	if err != nil {
		t.Fatalf("CreateAuction: %v", err)
	}
	return a
}

func TestCreateAuctionValidation(t *testing.T) {
	e, clk, _ := newTestEngine(t)
	deadline := clk.Now().Add(time.Hour)

	if _, err := e.CreateAuction(0, deadline, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("qty=0: err=%v, want ErrInvalidArgument", err)
	}
	if _, err := e.CreateAuction(-1, deadline, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("qty=-1: err=%v", err)
	}
	if _, err := e.CreateAuction(10, time.Time{}, 0); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("zero deadline: err=%v", err)
	}
	if _, err := e.CreateAuction(10, deadline, MustParseMoney("-0.0001")); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("negative reserve: err=%v", err)
	}
}

func TestSubmitBidValidationAndNotFound(t *testing.T) {
	e, clk, _ := newTestEngine(t)
	dl := clk.Now().Add(time.Hour)
	mustCreateAuction(t, e, 10, dl, "0")

	good := BidInput{AuctionID: 1, ExternalID: "x", Bidder: "A", Qty: 1, Price: MustParseMoney("1")}
	badExt, badBidder, badQty1, badQty2, badPrice := good, good, good, good, good
	badExt.ExternalID = ""
	badBidder.Bidder = ""
	badQty1.Qty = 0
	badQty2.Qty = -2
	badPrice.Price = MustParseMoney("-0.0001")
	cases := []BidInput{badExt, badBidder, badQty1, badQty2, badPrice}
	for i, in := range cases {
		if _, err := e.SubmitBid(in); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("case %d: err=%v, want ErrInvalidArgument", i, err)
		}
	}
	if _, err := e.SubmitBid(BidInput{AuctionID: 999, ExternalID: "x", Bidder: "A", Qty: 1, Price: 1}); !errors.Is(err, ErrAuctionNotFound) {
		t.Errorf("missing auction: err=%v", err)
	}
}

// 截止边界使用统一时钟：Now == Deadline 视为已截止。
func TestDeadlineBoundary(t *testing.T) {
	e, clk, _ := newTestEngine(t)
	dl := clk.Now().Add(time.Minute)
	mustCreateAuction(t, e, 10, dl, "0")

	in := BidInput{AuctionID: 1, ExternalID: "b1", Bidder: "A", Qty: 1, Price: MustParseMoney("10")}
	if _, err := e.SubmitBid(in); err != nil {
		t.Fatal(err)
	}

	clk.Set(dl.Add(-1)) // 截止前 1ns：仍可报价
	in.ExternalID = "b2"
	if _, err := e.SubmitBid(in); err != nil {
		t.Fatalf("1ns before deadline: %v", err)
	}

	clk.Set(dl) // 恰好等于截止点：拒绝
	in.ExternalID = "b3"
	if _, err := e.SubmitBid(in); !errors.Is(err, ErrDeadlinePassed) {
		t.Fatalf("at deadline: err=%v, want ErrDeadlinePassed", err)
	}
	if _, err := e.WithdrawBid(1, "b1"); !errors.Is(err, ErrDeadlinePassed) {
		t.Fatalf("withdraw at deadline: err=%v", err)
	}

	clk.Advance(time.Second) // 迟到报价不能进入快照
	in.ExternalID = "b4"
	if _, err := e.SubmitBid(in); !errors.Is(err, ErrDeadlinePassed) {
		t.Fatalf("late bid: err=%v", err)
	}
}

// 同外部编号：相同内容幂等返回原结果；不同内容冲突。
func TestSubmitBidIdempotencyAndConflict(t *testing.T) {
	e, clk, _ := newTestEngine(t)
	mustCreateAuction(t, e, 10, clk.Now().Add(time.Hour), "0")

	in := BidInput{AuctionID: 1, ExternalID: "ext-1", Bidder: "A", Qty: 5, Price: MustParseMoney("10.5")}
	r1, err := e.SubmitBid(in)
	if err != nil || r1.Outcome != BidAccepted {
		t.Fatalf("first submit: %+v err=%v", r1, err)
	}
	first := r1.Bid

	clk.Advance(time.Minute)
	r2, err := e.SubmitBid(in)
	if err != nil || r2.Outcome != BidReplayed {
		t.Fatalf("replay: %+v err=%v", r2, err)
	}
	// 必须返回原记录：首次提交时间不被覆盖。
	if r2.Bid.SubmittedAt != first.SubmittedAt || r2.Bid.UpdatedAt != first.UpdatedAt {
		t.Error("replay returned a different record")
	}

	mutBidder, mutQty, mutPrice := in, in, in
	mutBidder.Bidder = "B"
	mutQty.Qty = 6
	mutPrice.Price = MustParseMoney("10.6")
	for _, mut := range []BidInput{mutBidder, mutQty, mutPrice} {
		if _, err := e.SubmitBid(mut); !errors.Is(err, ErrBidConflict) {
			t.Errorf("mut=%+v err=%v, want ErrBidConflict", mut, err)
		}
	}
	// 冲突后原记录保持不变。
	got, _ := e.GetBid(1, "ext-1")
	if got.Bidder != "A" || got.Qty != 5 || got.Price != MustParseMoney("10.5") {
		t.Errorf("original bid mutated by conflicting request: %+v", got)
	}
}

// 截止后用相同内容重试：仍返回原结果（幂等优先于状态判断）。
func TestSubmitBidReplayAfterDeadline(t *testing.T) {
	e, clk, _ := newTestEngine(t)
	mustCreateAuction(t, e, 10, clk.Now().Add(time.Minute), "0")
	in := BidInput{AuctionID: 1, ExternalID: "ext-1", Bidder: "A", Qty: 5, Price: MustParseMoney("10")}
	if _, err := e.SubmitBid(in); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Minute)
	r, err := e.SubmitBid(in)
	if err != nil || r.Outcome != BidReplayed {
		t.Fatalf("post-deadline replay: %+v err=%v", r, err)
	}
	// 不同内容依然是冲突，而不是截止错误。
	conflict := in
	conflict.Qty = 9
	if _, err := e.SubmitBid(conflict); !errors.Is(err, ErrBidConflict) {
		t.Fatalf("post-deadline conflict: err=%v", err)
	}
}

// 撤回：成功、重复撤回幂等、撤回不存在报价。
func TestWithdraw(t *testing.T) {
	e, clk, _ := newTestEngine(t)
	mustCreateAuction(t, e, 10, clk.Now().Add(time.Hour), "0")
	in := BidInput{AuctionID: 1, ExternalID: "w1", Bidder: "A", Qty: 3, Price: MustParseMoney("10")}
	if _, err := e.SubmitBid(in); err != nil {
		t.Fatal(err)
	}

	if _, err := e.WithdrawBid(999, "w1"); !errors.Is(err, ErrAuctionNotFound) {
		t.Errorf("missing auction: err=%v", err)
	}
	if _, err := e.WithdrawBid(1, "nope"); !errors.Is(err, ErrBidNotFound) {
		t.Errorf("missing bid: err=%v", err)
	}

	r1, err := e.WithdrawBid(1, "w1")
	if err != nil || r1.Outcome != WithdrawDone || r1.Bid.Status != BidWithdrawn {
		t.Fatalf("first withdraw: %+v err=%v", r1, err)
	}
	withdrawnAt := r1.Bid.UpdatedAt

	clk.Advance(2 * time.Hour) // 已过截止点
	r2, err := e.WithdrawBid(1, "w1")
	if err != nil || r2.Outcome != WithdrawReplayed {
		t.Fatalf("replay withdraw: %+v err=%v", r2, err)
	}
	if r2.Bid.UpdatedAt != withdrawnAt {
		t.Error("replay withdraw changed the record timestamp")
	}

	// 已撤回的报价不能被相同内容的重复提交“恢复”。
	r3, err := e.SubmitBid(in)
	if err != nil || r3.Outcome != BidReplayed || r3.Bid.Status != BidWithdrawn {
		t.Fatalf("resubmit withdrawn bid: %+v err=%v", r3, err)
	}
}

// 完整清算流程 + 迟到报价不入快照 + 撤回不恢复 + 重复清算读原结果。
func TestSettleFlowAndReplay(t *testing.T) {
	e, clk, _ := newTestEngine(t)
	dl := clk.Now().Add(time.Hour)
	mustCreateAuction(t, e, 10, dl, "5")

	submit := func(ext, bidder string, qty int64, price string) {
		t.Helper()
		_, err := e.SubmitBid(BidInput{AuctionID: 1, ExternalID: ext, Bidder: bidder, Qty: qty, Price: MustParseMoney(price)})
		if err != nil {
			t.Fatal(err)
		}
	}
	submit("b1", "A", 4, "10")
	clk.Advance(time.Second)
	submit("b2", "B", 8, "9") // 边界：将部分成交 6
	clk.Advance(time.Second)
	submit("b3", "C", 2, "4") // 低于保留价
	clk.Advance(time.Second)
	if _, err := e.WithdrawBid(1, "b3"); err != nil {
		t.Fatal(err)
	}

	// 截止前不能清算。
	if _, err := e.SettleAuction(1); !errors.Is(err, ErrAuctionNotClosed) {
		t.Fatalf("early settle: err=%v", err)
	}

	clk.Set(dl)
	// 迟到报价。
	_, errLate := e.SubmitBid(BidInput{AuctionID: 1, ExternalID: "late", Bidder: "L", Qty: 100, Price: MustParseMoney("999")})
	if !errors.Is(errLate, ErrDeadlinePassed) {
		t.Fatalf("late submit: err=%v", errLate)
	}

	r1, err := e.SettleAuction(1)
	if err != nil || r1.Outcome != SettleSettled {
		t.Fatalf("first settle: %+v err=%v", r1, err)
	}
	s := r1.Data
	if s.SoldQty != 10 || s.ClearingPrice.String() != "9" || s.TotalProceeds.String() != "90" {
		t.Fatalf("settlement = %+v", s)
	}
	if len(s.Fills) != 2 || s.Fills[0].ExternalID != "b1" || s.Fills[0].FilledQty != 4 ||
		s.Fills[1].ExternalID != "b2" || s.Fills[1].FilledQty != 6 {
		t.Fatalf("fills = %+v", s.Fills)
	}
	if s.Fills[0].Price.String() != "9" {
		t.Errorf("uniform price not applied: %+v", s.Fills[0])
	}

	// 重复清算：读原结果。
	r2, err := e.SettleAuction(1)
	if err != nil || r2.Outcome != SettleReplayed {
		t.Fatalf("second settle: %+v err=%v", r2, err)
	}
	if r2.Data.SettledAt != s.SettledAt || r2.Data.TotalProceeds != s.TotalProceeds {
		t.Error("replayed settlement differs from original")
	}

	// 清算后报价/撤回均被拒绝。
	if _, err := e.SubmitBid(BidInput{AuctionID: 1, ExternalID: "x", Bidder: "A", Qty: 1, Price: MustParseMoney("1")}); !errors.Is(err, ErrAlreadySettled) {
		t.Errorf("post-settle submit: err=%v", err)
	}
	if _, err := e.WithdrawBid(1, "b1"); !errors.Is(err, ErrAlreadySettled) {
		t.Errorf("post-settle withdraw: err=%v", err)
	}

	// 成交结果查询。
	got, err := e.GetSettlement(1)
	if err != nil || got.SoldQty != 10 {
		t.Fatalf("GetSettlement: %+v err=%v", got, err)
	}
	f, err := e.GetFill(1, "b2")
	if err != nil || f.FilledQty != 6 {
		t.Fatalf("GetFill b2: %+v err=%v", f, err)
	}
	if _, err := e.GetFill(1, "b3"); !errors.Is(err, ErrFillNotFound) {
		t.Errorf("GetFill b3: err=%v", err)
	}
}

// 并发清算：无论多少个请求同时发起，恰好一个 settled，其余全部 replay 同一份结果。
func TestConcurrentSettlementSingleWinner(t *testing.T) {
	e, clk, _ := newTestEngine(t)
	dl := clk.Now().Add(time.Hour)
	mustCreateAuction(t, e, 5, dl, "0")
	for i := 0; i < 20; i++ {
		_, err := e.SubmitBid(BidInput{
			AuctionID: 1, ExternalID: fmt.Sprintf("b%02d", i), Bidder: "X",
			Qty: 1, Price: MustParseMoney(fmt.Sprintf("%d", 100-i)),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	clk.Set(dl)

	const n = 32
	var wg sync.WaitGroup
	results := make([]SettleOutcome, n)
	errs := make([]error, n)
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := e.SettleAuction(1)
			results[i], errs[i] = r.Outcome, err
		}(i)
	}
	close(start)
	wg.Wait()

	settled, replayed := 0, 0
	var first Settlement
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("concurrent settle error: %v", errs[i])
		}
		switch results[i] {
		case SettleSettled:
			settled++
		case SettleReplayed:
			replayed++
		}
	}
	if settled != 1 || replayed != n-1 {
		t.Fatalf("settled=%d replayed=%d, want 1/%d", settled, replayed, n-1)
	}
	first, _ = e.GetSettlement(1)
	if first.SoldQty != 5 || len(first.Fills) != 5 {
		t.Fatalf("unexpected fills: %+v", first.Fills)
	}
}

// 撤回恰好撞上清算：通过测试钩子把撤回精确注入“快照已固定、结果未提交”的窗口。
// 裁决必须唯一：快照不含撤回，撤回收到 ErrConcurrentSettlement，成交照常包含该报价。
func TestWithdrawRacingSettlement(t *testing.T) {
	e, clk, _ := newTestEngine(t)
	dl := clk.Now().Add(time.Hour)
	mustCreateAuction(t, e, 10, dl, "0")
	_, err := e.SubmitBid(BidInput{AuctionID: 1, ExternalID: "race", Bidder: "A", Qty: 3, Price: MustParseMoney("10")})
	if err != nil {
		t.Fatal(err)
	}
	clk.Set(dl)

	inside := make(chan struct{})
	release := make(chan struct{})
	e.testHook = func(auctionID int64, snapshot []Bid) {
		close(inside)
		<-release
	}

	settleErr := make(chan error, 1)
	settleRes := make(chan SettleResult, 1)
	go func() {
		r, err := e.SettleAuction(1)
		settleRes <- r
		settleErr <- err
	}()

	<-inside // 快照已固定
	wdErr := make(chan error, 1)
	go func() {
		_, err := e.WithdrawBid(1, "race")
		wdErr <- err
	}()

	// 撤回必须快速失败（而不是被阻塞或抢先改动快照）。
	select {
	case err := <-wdErr:
		if !errors.Is(err, ErrConcurrentSettlement) {
			t.Fatalf("withdraw during settlement window: err=%v, want ErrConcurrentSettlement", err)
		}
	case <-time.After(time.Second):
		t.Fatal("withdraw blocked during settlement window")
	}

	close(release) // 放行清算提交
	if err := <-settleErr; err != nil {
		t.Fatalf("settle: %v", err)
	}
	r := <-settleRes
	if r.Outcome != SettleSettled {
		t.Fatalf("settle outcome = %s", r.Outcome)
	}

	s, _ := e.GetSettlement(1)
	if len(s.Fills) != 1 || s.Fills[0].ExternalID != "race" || s.Fills[0].FilledQty != 3 {
		t.Fatalf("racing withdraw must not affect snapshot, fills=%+v", s.Fills)
	}
}

// 清算窗口内重复撤回一个此前已撤回的报价：纯幂等读，仍返回原结果而非并发冲突。
func TestReplayWithdrawDuringSettlementWindow(t *testing.T) {
	e, clk, _ := newTestEngine(t)
	dl := clk.Now().Add(time.Hour)
	mustCreateAuction(t, e, 10, dl, "0")
	_, _ = e.SubmitBid(BidInput{AuctionID: 1, ExternalID: "w", Bidder: "A", Qty: 3, Price: MustParseMoney("10")})
	if _, err := e.WithdrawBid(1, "w"); err != nil {
		t.Fatal(err)
	}
	clk.Set(dl)

	inside := make(chan struct{})
	release := make(chan struct{})
	e.testHook = func(auctionID int64, snapshot []Bid) {
		close(inside)
		<-release
	}
	go func() { _, _ = e.SettleAuction(1) }()
	<-inside

	r, err := e.WithdrawBid(1, "w")
	close(release)
	if err != nil || r.Outcome != WithdrawReplayed || r.Bid.Status != BidWithdrawn {
		t.Fatalf("replay withdraw in window: %+v err=%v", r, err)
	}
}

// 迟到报价撞上清算窗口：快速并发冲突，且不得混入快照。
func TestLateBidRacingSettlement(t *testing.T) {
	e, clk, _ := newTestEngine(t)
	dl := clk.Now().Add(time.Hour)
	mustCreateAuction(t, e, 10, dl, "0")
	_, _ = e.SubmitBid(BidInput{AuctionID: 1, ExternalID: "b1", Bidder: "A", Qty: 2, Price: MustParseMoney("10")})
	clk.Set(dl)

	inside := make(chan struct{})
	release := make(chan struct{})
	e.testHook = func(auctionID int64, snapshot []Bid) {
		close(inside)
		<-release
	}
	settleDone := make(chan struct{})
	go func() {
		_, _ = e.SettleAuction(1)
		close(settleDone)
	}()
	<-inside
	rr, err := e.SubmitBid(BidInput{AuctionID: 1, ExternalID: "b1", Bidder: "A", Qty: 2, Price: MustParseMoney("10")})
	if err != nil || rr.Outcome != BidReplayed {
		t.Fatalf("replay bid in window: %+v err=%v", rr, err)
	}

	submitErr := make(chan error, 1)
	go func() {
		_, err := e.SubmitBid(BidInput{AuctionID: 1, ExternalID: "late", Bidder: "L", Qty: 100, Price: MustParseMoney("999")})
		submitErr <- err
	}()
	select {
	case err := <-submitErr:
		if !errors.Is(err, ErrConcurrentSettlement) {
			t.Fatalf("late submit in settlement window: err=%v, want ErrConcurrentSettlement", err)
		}
	case <-time.After(time.Second):
		t.Fatal("late submit blocked during settlement window")
	}
	close(release)
	<-settleDone // 等待清算提交完成，避免读到提交前的状态

	s, _ := e.GetSettlement(1)
	if s.SoldQty != 2 {
		t.Fatalf("late bid leaked into snapshot: sold=%d", s.SoldQty)
	}
}

// 高并发混合负载下，成交总量与事件序列必须自洽。
func TestConcurrentMixedLoad(t *testing.T) {
	e, clk, _ := newTestEngine(t)
	dl := clk.Now().Add(time.Hour)
	mustCreateAuction(t, e, 100, dl, "0")

	var wg sync.WaitGroup
	start := make(chan struct{})
	// 20 个竞买方各自报价（唯一编号，不冲突）。
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _ = e.SubmitBid(BidInput{
				AuctionID:  1,
				ExternalID: fmt.Sprintf("b%02d", i),
				Bidder:     fmt.Sprintf("bidder-%02d", i),
				Qty:        int64(5 + i),
				Price:      MustParseMoney(fmt.Sprintf("%d", 100-i)),
			})
		}(i)
	}
	// 4 个清算请求在截止前并发触发——此时全部应得到 ErrAuctionNotClosed。
	earlyErrs := make([]error, 4)
	for i := range earlyErrs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, earlyErrs[i] = e.SettleAuction(1)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range earlyErrs {
		if !errors.Is(err, ErrAuctionNotClosed) {
			t.Errorf("early settler %d: err=%v", i, err)
		}
	}

	clk.Set(dl)
	r, err := e.SettleAuction(1)
	if err != nil {
		t.Fatal(err)
	}
	if r.Data.SoldQty > 100 {
		t.Fatalf("sold %d exceeds supply 100", r.Data.SoldQty)
	}
	// 同一份结果重复读取。
	r2, _ := e.SettleAuction(1)
	if r2.Outcome != SettleReplayed || r2.Data.TotalProceeds != r.Data.TotalProceeds {
		t.Fatal("replayed settlement mismatch")
	}
}

// 从同一事件存储重建引擎：状态与成交结果完全恢复。
func TestReplayFromStore(t *testing.T) {
	clk := NewControlledClock(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	store := NewMemoryEventStore()
	e1, err := NewEngine(store, clk)
	if err != nil {
		t.Fatal(err)
	}
	dl := clk.Now().Add(time.Hour)
	a := mustCreateAuction(t, e1, 7, dl, "3")
	_, _ = e1.SubmitBid(BidInput{AuctionID: a.ID, ExternalID: "b1", Bidder: "A", Qty: 5, Price: MustParseMoney("10")})
	_, _ = e1.SubmitBid(BidInput{AuctionID: a.ID, ExternalID: "b2", Bidder: "B", Qty: 5, Price: MustParseMoney("9")})
	_, _ = e1.WithdrawBid(a.ID, "b1")
	clk.Set(dl)
	s1, err := e1.SettleAuction(a.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 用新时钟重建：已落库的事实不受时钟影响。
	e2, err := NewEngine(store, SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	gotA, err := e2.GetAuction(a.ID)
	if err != nil || gotA.Status != AuctionSettled {
		t.Fatalf("auction after replay: %+v err=%v", gotA, err)
	}
	b, err := e2.GetBid(a.ID, "b1")
	if err != nil || b.Status != BidWithdrawn {
		t.Fatalf("withdrawn bid after replay: %+v err=%v", b, err)
	}
	s2, err := e2.GetSettlement(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s2.SoldQty != s1.Data.SoldQty || s2.ClearingPrice != s1.Data.ClearingPrice ||
		s2.TotalProceeds != s1.Data.TotalProceeds || len(s2.Fills) != len(s1.Data.Fills) {
		t.Fatalf("settlement after replay differs: %+v vs %+v", s2, s1.Data)
	}
	// b1 已撤回，成交的只有 b2 5 份。
	if len(s2.Fills) != 1 || s2.Fills[0].ExternalID != "b2" || s2.Fills[0].FilledQty != 5 {
		t.Fatalf("fills after replay: %+v", s2.Fills)
	}
	// 重建后重复清算仍是 replay。
	r, err := e2.SettleAuction(a.ID)
	if err != nil || r.Outcome != SettleReplayed {
		t.Fatalf("settle after replay: %+v err=%v", r, err)
	}
}

func TestGetNotFound(t *testing.T) {
	e, _, _ := newTestEngine(t)
	if _, err := e.GetAuction(42); !errors.Is(err, ErrAuctionNotFound) {
		t.Errorf("GetAuction: err=%v", err)
	}
	if _, err := e.GetSettlement(42); !errors.Is(err, ErrAuctionNotFound) {
		t.Errorf("GetSettlement missing auction: err=%v", err)
	}
	mustCreateAuction(t, e, 1, time.Now().Add(time.Hour), "0")
	if _, err := e.GetSettlement(1); !errors.Is(err, ErrSettlementNotFound) {
		t.Errorf("GetSettlement before settle: err=%v", err)
	}
	if !errors.Is(ErrSettlementNotFound, ErrNotFound) {
		t.Error("ErrSettlementNotFound should wrap ErrNotFound")
	}
}
