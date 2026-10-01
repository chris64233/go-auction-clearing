package auctionclearing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// sameResult 比较两份清算结果的内容是否逐字段一致
// （仓储按契约返回深拷贝，故比较内容而非指针同一性）。
func sameResult(r1, r2 *ClearingResult) bool {
	d1, e1 := json.Marshal(r1)
	d2, e2 := json.Marshal(r2)
	return e1 == nil && e2 == nil && string(d1) == string(d2)
}

// createTestAuction 通过服务创建一场截止点为 now+1h 的拍卖。
func createTestAuction(t *testing.T, svc *Service) *Auction {
	t.Helper()
	return createTestAuctionDeadline(t, svc, time.Hour)
}

func createTestAuctionDeadline(t *testing.T, svc *Service, d time.Duration) *Auction {
	t.Helper()
	a, err := svc.CreateAuction(context.Background(), CreateAuctionParams{
		AvailableQty: 10,
		Deadline:     svc.clock.Now().Add(d),
		ReservePrice: MustParseMoney("5"),
	})
	if err != nil {
		t.Fatalf("create auction: %v", err)
	}
	return a
}

func bidParams(ref string, qty int64, price string) PlaceBidParams {
	return PlaceBidParams{ExternalRef: ref, Bidder: "bidder-" + ref, Quantity: qty, UnitPrice: MustParseMoney(price)}
}

func TestCreateAuctionValidation(t *testing.T) {
	svc := NewService(NewMemoryRepository(), newFakeClock())
	_, err := svc.CreateAuction(context.Background(), CreateAuctionParams{
		AvailableQty: 0, Deadline: time.Now().Add(time.Hour), ReservePrice: MustParseMoney("1"),
	})
	if ErrorKindOf(err) != KindInvalidArgument {
		t.Fatalf("qty 0: kind = %v, want invalid_argument", ErrorKindOf(err))
	}
	a, err := svc.GetAuction(context.Background(), "missing")
	if a != nil || ErrorKindOf(err) != KindNotFound {
		t.Fatalf("missing auction: a=%v kind=%v", a, ErrorKindOf(err))
	}
}

func TestPlaceBid_IdempotentReplay(t *testing.T) {
	clock := newFakeClock()
	svc := NewService(NewMemoryRepository(), clock)
	a := createTestAuction(t, svc)
	p := bidParams("ORD-1", 3, "10.50")

	out1, err := svc.PlaceBid(context.Background(), a.ID, p)
	if err != nil || out1.Replayed {
		t.Fatalf("first place: out=%+v err=%v", out1, err)
	}
	firstSeen := out1.Bid.FirstSubmittedAt

	clock.Advance(time.Minute)
	out2, err := svc.PlaceBid(context.Background(), a.ID, p)
	if err != nil {
		t.Fatalf("replay error: %v", err)
	}
	if !out2.Replayed {
		t.Error("identical re-submission must be marked replayed")
	}
	// 首次提交时间不得因重放而改变。
	if !out2.Bid.FirstSubmittedAt.Equal(firstSeen) {
		t.Error("FirstSubmittedAt changed on idempotent replay")
	}
	// 返回的必须是同一份内容。
	if out2.Bid.Quantity != 3 || out2.Bid.UnitPrice.String() != "10.5" {
		t.Error("replayed bid content mismatch")
	}
}

func TestPlaceBid_ConflictOnDifferentContent(t *testing.T) {
	svc := NewService(NewMemoryRepository(), newFakeClock())
	a := createTestAuction(t, svc)
	if _, err := svc.PlaceBid(context.Background(), a.ID, bidParams("ORD-1", 3, "10")); err != nil {
		t.Fatal(err)
	}
	// 改数量
	_, err := svc.PlaceBid(context.Background(), a.ID, bidParams("ORD-1", 4, "10"))
	if ErrorKindOf(err) != KindConflict {
		t.Fatalf("change qty: kind=%v want conflict", ErrorKindOf(err))
	}
	// 改单价
	_, err = svc.PlaceBid(context.Background(), a.ID, PlaceBidParams{
		ExternalRef: "ORD-1", Bidder: "bidder-ORD-1", Quantity: 3, UnitPrice: MustParseMoney("11"),
	})
	if ErrorKindOf(err) != KindConflict {
		t.Fatalf("change price: kind=%v want conflict", ErrorKindOf(err))
	}
	// 改竞买方
	_, err = svc.PlaceBid(context.Background(), a.ID, PlaceBidParams{
		ExternalRef: "ORD-1", Bidder: "someone-else", Quantity: 3, UnitPrice: MustParseMoney("10"),
	})
	if ErrorKindOf(err) != KindConflict {
		t.Fatalf("change bidder: kind=%v want conflict", ErrorKindOf(err))
	}
	// 冲突写入不得污染原报价。
	b, _ := svc.GetBid(context.Background(), a.ID, "ORD-1")
	if b.Quantity != 3 || b.UnitPrice.String() != "10" {
		t.Fatal("original bid altered by conflicting submission")
	}
}

func TestPlaceBid_DeadlineBoundary(t *testing.T) {
	clock := newFakeClock()
	svc := NewService(NewMemoryRepository(), clock)
	deadline := clock.Now().Add(time.Hour)
	a, err := svc.CreateAuction(context.Background(), CreateAuctionParams{
		AvailableQty: 10, Deadline: deadline, ReservePrice: MustParseMoney("1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// now == deadline：恰好落在截止点上的报价必须被拒绝（边界归属截止后）。
	clock.Set(deadline)
	_, err = svc.PlaceBid(context.Background(), a.ID, bidParams("LATE", 1, "10"))
	if !errors.Is(err, ErrBiddingClosed) {
		t.Fatalf("bid at exact deadline: err=%v want ErrBiddingClosed", err)
	}
	// 截止前一刻可以报价。
	clock.Set(deadline.Add(-time.Nanosecond))
	if _, err := svc.PlaceBid(context.Background(), a.ID, bidParams("OK", 1, "10")); err != nil {
		t.Fatalf("bid just before deadline: %v", err)
	}
	// 迟到报价不得进入快照：清算后结果为空。
	clock.Set(deadline.Add(time.Minute))
	if _, err := svc.ClearAuction(context.Background(), a.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	res, _ := svc.GetResult(context.Background(), a.ID)
	if res.BidCount != 1 || res.SoldQty != 1 {
		t.Fatalf("late bid leaked into snapshot: count=%d sold=%d", res.BidCount, res.SoldQty)
	}
}

func TestWithdraw_ThenReplayDoesNotRestore(t *testing.T) {
	clock := newFakeClock()
	svc := NewService(NewMemoryRepository(), clock)
	a := createTestAuction(t, svc)
	if _, err := svc.PlaceBid(context.Background(), a.ID, bidParams("ORD-1", 2, "10")); err != nil {
		t.Fatal(err)
	}
	w1, err := svc.WithdrawBid(context.Background(), a.ID, "ORD-1")
	if err != nil || w1.Replayed {
		t.Fatalf("first withdraw: %+v %v", w1, err)
	}
	if w1.Bid.State != BidWithdrawn || w1.Bid.WithdrawnAt == nil {
		t.Fatal("bid not marked withdrawn")
	}
	// 重复撤回报原状态。
	w2, err := svc.WithdrawBid(context.Background(), a.ID, "ORD-1")
	if err != nil || !w2.Replayed || w2.Bid.State != BidWithdrawn {
		t.Fatalf("replay withdraw: %+v %v", w2, err)
	}
	// 用相同内容重新提交该外部编号：只能是幂等重放，已撤回状态不得被恢复。
	p, err := svc.PlaceBid(context.Background(), a.ID, bidParams("ORD-1", 2, "10"))
	if err != nil || !p.Replayed || p.Bid.State != BidWithdrawn {
		t.Fatalf("resubmit after withdraw must replay withdrawn state: %+v %v", p, err)
	}
	// 清算快照中不应有它。
	clock.Advance(time.Hour + time.Minute)
	if _, err := svc.ClearAuction(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	res, _ := svc.GetResult(context.Background(), a.ID)
	if res.BidCount != 0 || res.SoldQty != 0 {
		t.Fatalf("withdrawn bid restored into snapshot: %+v", res)
	}
}

func TestWithdraw_AfterDeadlineRejected(t *testing.T) {
	clock := newFakeClock()
	svc := NewService(NewMemoryRepository(), clock)
	a := createTestAuction(t, svc)
	if _, err := svc.PlaceBid(context.Background(), a.ID, bidParams("ORD-1", 2, "10")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour) // 到达截止点
	if _, err := svc.WithdrawBid(context.Background(), a.ID, "ORD-1"); !errors.Is(err, ErrBiddingClosed) {
		t.Fatalf("withdraw at deadline: err=%v want closed", err)
	}
	b, _ := svc.GetBid(context.Background(), a.ID, "ORD-1")
	if b.State != BidActive {
		t.Fatal("rejected withdraw must not change bid state")
	}
}

func TestWithdraw_MissingBid(t *testing.T) {
	svc := NewService(NewMemoryRepository(), newFakeClock())
	a := createTestAuction(t, svc)
	_, err := svc.WithdrawBid(context.Background(), a.ID, "nope")
	if ErrorKindOf(err) != KindNotFound {
		t.Fatalf("kind = %v, want not_found", ErrorKindOf(err))
	}
}

func TestClear_BeforeDeadlineRejected(t *testing.T) {
	svc := NewService(NewMemoryRepository(), newFakeClock())
	a := createTestAuction(t, svc)
	_, err := svc.ClearAuction(context.Background(), a.ID)
	if !errors.Is(err, ErrNotYetClosed) {
		t.Fatalf("early clear: err=%v want ErrNotYetClosed", err)
	}
	// 被拒绝的清算不得留下任何成交痕迹。
	if _, err := svc.GetResult(context.Background(), a.ID); ErrorKindOf(err) != KindNotFound {
		t.Fatalf("result after rejected clear: %v", err)
	}
	if got, _ := svc.GetAuction(context.Background(), a.ID); got.Status != StatusOpen {
		t.Fatal("auction must remain open after rejected clear")
	}
}

func TestClear_OnceAndRepeatedReads(t *testing.T) {
	clock := newFakeClock()
	svc := NewService(NewMemoryRepository(), clock)
	a := createTestAuction(t, svc)
	if _, err := svc.PlaceBid(context.Background(), a.ID, bidParams("ORD-1", 8, "10")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Hour)

	first, err := svc.ClearAuction(context.Background(), a.ID)
	if err != nil || first.Replayed {
		t.Fatalf("first clear: %+v %v", first, err)
	}
	if first.Result.SoldQty != 8 {
		t.Fatalf("sold = %d, want 8", first.Result.SoldQty)
	}
	// 重复请求读到同一份原结果。
	second, err := svc.ClearAuction(context.Background(), a.ID)
	if err != nil || !second.Replayed {
		t.Fatalf("second clear must replay: %+v %v", second, err)
	}
	if !sameResult(second.Result, first.Result) {
		t.Error("replayed result must equal the stored result")
	}
	// 清算后报价/撤回均被拒绝。
	if _, err := svc.PlaceBid(context.Background(), a.ID, bidParams("NEW", 1, "10")); !errors.Is(err, ErrAuctionCleared) {
		t.Errorf("bid after clear: %v", err)
	}
	if _, err := svc.WithdrawBid(context.Background(), a.ID, "ORD-1"); !errors.Is(err, ErrAuctionCleared) {
		t.Errorf("withdraw after clear: %v", err)
	}
}

func TestClear_WithdrawRacingClearIsUnique(t *testing.T) {
	// 用可挂起的仓储在某操作“已完成判定与修改、尚未提交”（仍持有拍卖锁）时
	// 阻塞它，再让竞争操作排队抢同一把锁，确定性地复现撤回与清算相撞。
	// 无论谁先提交，结局都必须唯一，且拍卖只清算一次、不超卖。
	t.Run("withdraw commits before clear", func(t *testing.T) {
		clock := newFakeClock()
		base := NewMemoryRepository()
		setupSvc := NewService(base, clock)
		a, err := setupSvc.CreateAuction(context.Background(), CreateAuctionParams{
			AvailableQty: 10, Deadline: clock.Now().Add(time.Hour), ReservePrice: MustParseMoney("1"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := setupSvc.PlaceBid(context.Background(), a.ID, bidParams("ORD-1", 5, "10")); err != nil {
			t.Fatal(err)
		}

		racer := newRacingRepository(base)
		armed := racer.arm(a.ID) // 第一次 Update（撤回）在提交前挂起
		withdrawSvc := NewService(racer, clock)
		clearSvc := NewService(racer, clock)

		werrCh := make(chan error, 1)
		go func() {
			_, e := withdrawSvc.WithdrawBid(context.Background(), a.ID, "ORD-1")
			werrCh <- e
		}()
		<-armed.barrier // 撤回已在锁内改好状态、尚未提交

		type clearResult struct {
			out *ClearOutcome
			err error
		}
		clearCh := make(chan clearResult, 1)
		go func() {
			clock.Advance(2 * time.Hour) // 已过截止点（两个 goroutine 之外推进，安全）
			out, e := clearSvc.ClearAuction(context.Background(), a.ID)
			clearCh <- clearResult{out, e}
		}()
		time.Sleep(20 * time.Millisecond) // 确保清算已排队阻塞在锁上
		close(armed.release)              // 放行撤回，使其先提交

		if werr := <-werrCh; werr != nil {
			t.Fatalf("withdraw should win, got %v", werr)
		}
		cr := <-clearCh
		if cr.err != nil {
			t.Fatalf("queued clear error: %v", cr.err)
		}
		if cr.out.Result.BidCount != 0 || cr.out.Result.SoldQty != 0 {
			t.Fatalf("withdraw-first: bid leaked into snapshot: %+v", cr.out.Result)
		}
		agg, _ := base.Load(context.Background(), a.ID)
		assertExactlyOnceCleared(t, agg)
	})

	t.Run("clear commits before withdraw", func(t *testing.T) {
		clock := newFakeClock()
		base := NewMemoryRepository()
		setupSvc := NewService(base, clock)
		deadline := clock.Now().Add(time.Hour)
		a, err := setupSvc.CreateAuction(context.Background(), CreateAuctionParams{
			AvailableQty: 10, Deadline: deadline, ReservePrice: MustParseMoney("1"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := setupSvc.PlaceBid(context.Background(), a.ID, bidParams("ORD-1", 5, "10")); err != nil {
			t.Fatal(err)
		}
		clock.Set(deadline.Add(time.Minute))

		racer := newRacingRepository(base)
		armed := racer.arm(a.ID) // 第一次 Update（清算）在提交前挂起
		clearSvc := NewService(racer, clock)
		withdrawSvc := NewService(racer, clock)

		clearCh := make(chan error, 1)
		go func() {
			_, e := clearSvc.ClearAuction(context.Background(), a.ID)
			clearCh <- e
		}()
		<-armed.barrier // 清算已在锁内生成结果、尚未提交

		werrCh := make(chan error, 1)
		go func() {
			_, e := withdrawSvc.WithdrawBid(context.Background(), a.ID, "ORD-1")
			werrCh <- e
		}()
		time.Sleep(20 * time.Millisecond) // 确保撤回已排队阻塞在锁上
		close(armed.release)              // 放行清算，使其先提交

		if cerr := <-clearCh; cerr != nil {
			t.Fatalf("clear should win, got %v", cerr)
		}
		if werr := <-werrCh; !errors.Is(werr, ErrAuctionCleared) {
			t.Fatalf("losing withdraw must fail ErrAuctionCleared, got %v", werr)
		}
		agg, _ := base.Load(context.Background(), a.ID)
		assertExactlyOnceCleared(t, agg)
		if agg.Clearing.SoldQty != 5 {
			t.Fatalf("clear-first: snapshot should contain the bid, sold=%d", agg.Clearing.SoldQty)
		}
		if agg.Bids["ORD-1"].State != BidActive {
			t.Fatal("losing withdraw must not mutate the bid")
		}
	})
}

func assertExactlyOnceCleared(t *testing.T, agg *Aggregate) {
	t.Helper()
	if agg.Auction.Status != StatusCleared || agg.Clearing == nil {
		t.Fatal("auction is not exactly-once cleared")
	}
	var sum int64
	for _, f := range agg.Clearing.Fills {
		sum += f.Quantity
	}
	if sum != agg.Clearing.SoldQty || sum > agg.Auction.AvailableQty {
		t.Fatalf("fills inconsistent: sum=%d sold=%d avail=%d",
			sum, agg.Clearing.SoldQty, agg.Auction.AvailableQty)
	}
}

// armPoint 标识一次“提交前挂起”的插入点。
type armPoint struct {
	barrier chan struct{} // fn 已执行完、锁仍持有时关闭
	release chan struct{} // 关闭后操作才继续提交
}

// racingRepository 包装仓储：指定拍卖的第一次 Update 在“修改完成、提交之前”
// （仍持有拍卖锁）于 armPoint 上同步，供测试制造确定性的并发交错。
type racingRepository struct {
	Repository
	points sync.Map // auctionID -> *armPoint
}

func newRacingRepository(base Repository) *racingRepository {
	return &racingRepository{Repository: base}
}

func (r *racingRepository) arm(auctionID string) *armPoint {
	p := &armPoint{barrier: make(chan struct{}), release: make(chan struct{})}
	r.points.Store(auctionID, p)
	return p
}

func (r *racingRepository) Update(ctx context.Context, id string, fn func(*Aggregate) error) (*Aggregate, error) {
	var p *armPoint
	if v, ok := r.points.LoadAndDelete(id); ok {
		p = v.(*armPoint)
	}
	wrapped := fn
	if p != nil {
		wrapped = func(agg *Aggregate) error {
			if err := fn(agg); err != nil {
				return err
			}
			// 此时仍处在底层仓储的锁内、提交之前。
			close(p.barrier)
			<-p.release
			return nil
		}
	}
	return r.Repository.Update(ctx, id, wrapped)
}

func TestClear_ConcurrentRequestsExactlyOnce(t *testing.T) {
	clock := newFakeClock()
	repo := NewMemoryRepository()
	svc := NewService(repo, clock)
	a := createTestAuction(t, svc)
	for i := 0; i < 50; i++ {
		if _, err := svc.PlaceBid(context.Background(), a.ID,
			bidParams(fmt.Sprintf("ORD-%02d", i), 1, "10")); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2 * time.Hour)

	const n = 32
	var wg sync.WaitGroup
	results := make([]*ClearingResult, n)
	replayed := make([]bool, n)
	errs := make([]error, n)
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			out, err := svc.ClearAuction(context.Background(), a.ID)
			if err == nil {
				results[i] = out.Result
				replayed[i] = out.Replayed
			}
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	firsts := 0
	var canonical *ClearingResult
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent clear error: %v", err)
		}
		if !replayed[i] {
			firsts++
		}
		if canonical == nil {
			canonical = results[i]
		} else if !sameResult(results[i], canonical) {
			t.Fatal("concurrent clears produced different results; duplicate fills possible")
		}
	}
	if firsts != 1 {
		t.Fatalf("exactly one first clear expected, got %d", firsts)
	}
	if canonical.SoldQty != 10 || len(canonical.Fills) != 10 {
		t.Fatalf("unexpected fills: sold=%d fills=%d", canonical.SoldQty, len(canonical.Fills))
	}
	// 成交数量合计不得超过可售量。
	var sum int64
	for _, f := range canonical.Fills {
		sum += f.Quantity
	}
	if sum != 10 {
		t.Fatalf("total filled = %d, want 10", sum)
	}
}
