package auctionclearing

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// failingGateway 是可切换失败的补偿结算通道；armBlock 后第一次结算会阻塞，
// 直到 release，用于精确制造“确认/冻结/交割与批准两阶段窗口并发”的交错。
type failingGateway struct {
	mu          sync.Mutex
	fail        bool
	entered     chan struct{}
	proceed     chan struct{}
	blockActive bool // 闩锁已装配且尚未 release
	used        bool // 本次闩锁是否已有补偿调用进入（只阻塞第一个）
}

func newFailingGateway(fail bool) *failingGateway {
	return &failingGateway{fail: fail}
}

func (g *failingGateway) setFail(fail bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fail = fail
}

func (g *failingGateway) armBlock() {
	g.entered = make(chan struct{})
	g.proceed = make(chan struct{})
	g.blockActive = true
	g.used = false
}

func (g *failingGateway) release() {
	g.mu.Lock()
	if g.blockActive {
		g.blockActive = false
		close(g.proceed)
	}
	g.mu.Unlock()
}

func (g *failingGateway) waitEntered() {
	<-g.entered
}

func (g *failingGateway) SettleCompensation(int64, string, Money) error {
	g.mu.Lock()
	fail := g.fail
	block := g.blockActive && !g.used
	entered := g.entered
	proceed := g.proceed
	if block {
		// 只阻塞本次闩锁的第一个补偿调用；后续调用直接通过。
		g.used = true
	}
	g.mu.Unlock()
	if block {
		if entered != nil {
			select {
			case <-entered:
			default:
				close(entered)
			}
		}
		<-proceed
	}
	if fail {
		return errors.New("compensation channel unavailable")
	}
	return nil
}

// correctionFixture 搭好一场已清算并生成首版结算的拍卖：
// 可售 10，报价 b1=A 3@10、b2=B 5@9、b3=C 4@8（时间错开），
// 截止后统一价清算（边界价 8）：b1 成交 3、b2 成交 5、b3 部分成交 2。
func correctionFixture(t *testing.T, gw CompensationGateway) (*Engine, *ControlledClock, int64) {
	t.Helper()
	t0 := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	clk := NewControlledClock(t0)
	var opts []EngineOption
	if gw != nil {
		opts = append(opts, WithCompensationGateway(gw))
	}
	e, err := NewEngine(NewMemoryEventStore(), clk, opts...)
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.CreateAuction(10, t0.Add(time.Hour), MustParseMoney("5"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []BidInput{
		{AuctionID: a.ID, ExternalID: "b1", Bidder: "A", Qty: 3, Price: MustParseMoney("10")},
		{AuctionID: a.ID, ExternalID: "b2", Bidder: "B", Qty: 5, Price: MustParseMoney("9")},
		{AuctionID: a.ID, ExternalID: "b3", Bidder: "C", Qty: 4, Price: MustParseMoney("8")},
	} {
		clk.Advance(time.Second)
		if _, err := e.SubmitBid(b); err != nil {
			t.Fatal(err)
		}
	}
	clk.Set(t0.Add(2 * time.Hour))
	if _, err := e.SettleAuction(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.GenerateSettlement(a.ID); err != nil {
		t.Fatal(err)
	}
	return e, clk, a.ID
}

func correctionItem(ext, target string) CorrectionItem {
	return CorrectionItem{ExternalID: ext, TargetUnitPrice: MustParseMoney(target)}
}

func correctionInput(cid string, items ...CorrectionItem) CorrectionInput {
	return CorrectionInput{
		CorrectionID: cid,
		Reason:       "upstream feed published a wrong clearing price",
		Basis:        "feed-v2-reconciliation-2026-09-01",
		Handler:      "risk-officer-1",
		Items:        items,
	}
}

func submitCorrectionFor(t *testing.T, e *Engine, auctionID int64, cid string, items ...CorrectionItem) CorrectionApplication {
	t.Helper()
	res, err := e.SubmitCorrection(auctionID, correctionInput(cid, items...))
	if err != nil {
		t.Fatalf("submit correction %s: %v", cid, err)
	}
	if res.Outcome != CorrectionAccepted {
		t.Fatalf("outcome = %s, want accepted", res.Outcome)
	}
	return res.Data
}

func approveCorrectionFor(t *testing.T, e *Engine, auctionID int64, cid string) CorrectionApplication {
	t.Helper()
	res, err := e.ApproveCorrection(auctionID, cid, ApproveCorrectionInput{})
	if err != nil {
		t.Fatalf("approve correction %s: %v", cid, err)
	}
	if res.Outcome != CorrectionAccepted {
		t.Fatalf("outcome = %s, want accepted", res.Outcome)
	}
	return res.Data
}
