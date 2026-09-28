package auctionclearing

import (
	"sort"
	"sync"
	"time"
)

// Engine 是拍卖聚合的并发安全内存内核，状态全部由 EventStore 重放得到。
//
// 并发模型（一把互斥锁 + 条件变量）：
//   - 所有读写字段都在 mu 保护下进行；时间一律取自统一 Clock。
//   - 清算分两阶段：先在锁内“认领”拍卖（置 settling 标记）并复制报价快照，
//     再在锁外执行纯函数撮合，最后回锁把【一个】EvSettled 事件原子追加落盘。
//   - 认领期间到达的报价/撤回立即得到 ErrConcurrentSettlement（快照已固定，
//     请求不可能再影响结果，裁决唯一确定，调用方应改查成交结果而非重试）；
//     并发的其他清算请求则在条件变量上等待，结束后读取原结果，绝不产生第二笔成交。
type Engine struct {
	mu   sync.Mutex
	cond *sync.Cond

	store EventStore
	clock Clock

	nextAuctionID int64
	auctions      map[int64]*Auction
	bids          map[int64]map[string]*Bid // auctionID -> externalID -> bid
	settlements   map[int64]*Settlement
	settling      map[int64]struct{}

	// testHook 仅供同包测试使用：快照已固定且锁已释放、撮合结果尚未提交时调用。
	// 生产环境始终为 nil，零开销。
	testHook func(auctionID int64, snapshot []Bid)
}

// NewEngine 从 EventStore 重放全部事件构建引擎；重放失败（日志损坏等）返回错误。
func NewEngine(store EventStore, clock Clock) (*Engine, error) {
	e := &Engine{
		store:       store,
		clock:       clock,
		auctions:    make(map[int64]*Auction),
		bids:        make(map[int64]map[string]*Bid),
		settlements: make(map[int64]*Settlement),
		settling:    make(map[int64]struct{}),
	}
	e.cond = sync.NewCond(&e.mu)
	if err := e.replay(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) replay() error {
	events, err := e.store.Load()
	if err != nil {
		return err
	}
	for _, ev := range events {
		switch ev.Type {
		case EvAuctionCreated:
			deadline := time.Time{}
			if ev.Deadline != nil {
				deadline = *ev.Deadline
			}
			a := &Auction{
				ID:           ev.AuctionID,
				AvailableQty: ev.AvailableQty,
				Deadline:     deadline,
				ReservePrice: ev.ReservePrice,
				Status:       AuctionOpen,
				CreatedAt:    ev.Timestamp,
			}
			e.auctions[a.ID] = a
			e.bids[a.ID] = make(map[string]*Bid)
			if a.ID > e.nextAuctionID {
				e.nextAuctionID = a.ID
			}
		case EvBidSubmitted:
			b := &Bid{
				AuctionID:   ev.AuctionID,
				ExternalID:  ev.ExternalID,
				Bidder:      ev.Bidder,
				Qty:         ev.Qty,
				Price:       ev.Price,
				SubmittedAt: ev.Timestamp,
				UpdatedAt:   ev.Timestamp,
				Status:      BidActive,
			}
			e.bids[ev.AuctionID][b.ExternalID] = b
		case EvBidWithdrawn:
			if b, ok := e.bids[ev.AuctionID][ev.ExternalID]; ok {
				b.Status = BidWithdrawn
				b.UpdatedAt = ev.Timestamp
			}
		case EvSettled:
			if ev.Settlement != nil {
				s := *ev.Settlement
				s.AuctionID = ev.AuctionID
				e.settlements[ev.AuctionID] = &s
				if a, ok := e.auctions[ev.AuctionID]; ok {
					a.Status = AuctionSettled
				}
			}
		}
	}
	return nil
}

// CreateAuction 创建一场拍卖：记录可售数量、报价截止点与最低成交价。
func (e *Engine) CreateAuction(availableQty int64, deadline time.Time, reservePrice Money) (Auction, error) {
	if availableQty <= 0 {
		return Auction{}, ErrAvailableQtyNotPositive
	}
	if deadline.IsZero() {
		return Auction{}, ErrZeroDeadline
	}
	if reservePrice.IsNegative() {
		return Auction{}, ErrNegativeReservePrice
	}

	now := e.clock.Now()

	e.mu.Lock()
	defer e.mu.Unlock()

	id := e.nextAuctionID + 1
	dl := deadline
	ev := Event{
		Type:         EvAuctionCreated,
		Timestamp:    now,
		AuctionID:    id,
		AvailableQty: availableQty,
		Deadline:     &dl,
		ReservePrice: reservePrice,
	}
	if _, err := e.store.Append([]Event{ev}); err != nil {
		return Auction{}, err
	}
	a := &Auction{
		ID:           id,
		AvailableQty: availableQty,
		Deadline:     deadline,
		ReservePrice: reservePrice,
		Status:       AuctionOpen,
		CreatedAt:    now,
	}
	e.auctions[id] = a
	e.bids[id] = make(map[string]*Bid)
	e.nextAuctionID = id
	return *a, nil
}

// SubmitBid 在报价窗口内提交报价。
//
// 幂等语义：同一拍卖内同一外部编号
//   - 内容（竞买方/数量/单价）完全一致：返回原记录（BidReplayed），
//     无论原记录当前是有效还是已撤回（撤回不可被重复提交恢复）；
//   - 内容不一致：返回 ErrBidConflict。
func (e *Engine) SubmitBid(in BidInput) (BidResult, error) {
	if in.ExternalID == "" {
		return BidResult{}, ErrEmptyExternalID
	}
	if in.Bidder == "" {
		return BidResult{}, ErrEmptyBidder
	}
	if in.Qty <= 0 {
		return BidResult{}, ErrBidQtyNotPositive
	}
	if in.Price.IsNegative() {
		return BidResult{}, ErrNegativePrice
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for {
		a, ok := e.auctions[in.AuctionID]
		if !ok {
			return BidResult{}, ErrAuctionNotFound
		}

		// 幂等判定优先且不受认领窗口影响：重复请求只读不写，
		// 任何时刻同一内容都稳定返回原结果，不同内容稳定冲突。
		if existing, exists := e.bids[in.AuctionID][in.ExternalID]; exists {
			if existing.Bidder == in.Bidder && existing.Qty == in.Qty && existing.Price == in.Price {
				return BidResult{Outcome: BidReplayed, Bid: *existing}, nil
			}
			return BidResult{}, ErrBidConflict
		}

		// 新报价恰好撞上清算认领窗口：快照已固定，本报价不可能进入，
		// 立即返回并发冲突；结果由即将落盘的清算唯一决定，调用方应改查成交结果。
		if _, busy := e.settling[in.AuctionID]; busy {
			return BidResult{}, ErrConcurrentSettlement
		}
		if a.Status == AuctionSettled {
			return BidResult{}, ErrAlreadySettled
		}
		now := e.clock.Now()
		if !now.Before(a.Deadline) {
			return BidResult{}, ErrDeadlinePassed // Now == Deadline 即截止
		}

		ev := Event{
			Type:       EvBidSubmitted,
			Timestamp:  now,
			AuctionID:  in.AuctionID,
			ExternalID: in.ExternalID,
			Bidder:     in.Bidder,
			Qty:        in.Qty,
			Price:      in.Price,
		}
		if _, err := e.store.Append([]Event{ev}); err != nil {
			return BidResult{}, err
		}
		b := &Bid{
			AuctionID:   in.AuctionID,
			ExternalID:  in.ExternalID,
			Bidder:      in.Bidder,
			Qty:         in.Qty,
			Price:       in.Price,
			SubmittedAt: now,
			UpdatedAt:   now,
			Status:      BidActive,
		}
		e.bids[in.AuctionID][in.ExternalID] = b
		return BidResult{Outcome: BidAccepted, Bid: *b}, nil
	}
}

// WithdrawBid 在报价窗口内撤回报价。
//
// 幂等语义：该编号此前已撤回时，重复请求直接返回原状态（WithdrawReplayed），
// 不受此后截止/清算影响；撤回恰好撞上清算认领窗口（快照已固定）时，
// 返回 ErrConcurrentSettlement——该报价按有效进入快照已成定局，调用方应改查
// 成交结果，而不是重试。
func (e *Engine) WithdrawBid(auctionID int64, externalID string) (WithdrawResult, error) {
	if externalID == "" {
		return WithdrawResult{}, ErrEmptyExternalID
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for {
		a, ok := e.auctions[auctionID]
		if !ok {
			return WithdrawResult{}, ErrAuctionNotFound
		}

		existing, exists := e.bids[auctionID][externalID]
		// 已撤回是终态：任何时候重试都返回原结果，保证调用方重试安全，
		// 且该纯读判定优先于认领窗口与截止判断。
		if exists && existing.Status == BidWithdrawn {
			return WithdrawResult{Outcome: WithdrawReplayed, Bid: *existing}, nil
		}

		// 有效报价的撤回恰好撞上清算认领窗口：快照里该报价按有效处理已成定局，
		// 撤回无法改变结果，立即返回并发冲突（而不是无限等待）。
		if _, busy := e.settling[auctionID]; busy {
			return WithdrawResult{}, ErrConcurrentSettlement
		}
		if a.Status == AuctionSettled {
			return WithdrawResult{}, ErrAlreadySettled
		}
		now := e.clock.Now()
		if !now.Before(a.Deadline) {
			return WithdrawResult{}, ErrDeadlinePassed
		}
		if !exists {
			return WithdrawResult{}, ErrBidNotFound
		}

		ev := Event{
			Type:       EvBidWithdrawn,
			Timestamp:  now,
			AuctionID:  auctionID,
			ExternalID: externalID,
		}
		if _, err := e.store.Append([]Event{ev}); err != nil {
			return WithdrawResult{}, err
		}
		existing.Status = BidWithdrawn
		existing.UpdatedAt = now
		return WithdrawResult{Outcome: WithdrawDone, Bid: *existing}, nil
	}
}

// SettleAuction 触发一场拍卖的原子清算。
//
//   - 截止点之前请求清算：ErrAuctionNotClosed；
//   - 已清算：返回原结果（SettleReplayed），重复请求永不产生第二笔成交；
//   - 并发清算：只有一个请求能完成认领并落盘，其余请求等待后读取原结果；
//   - 撮合或落盘失败：不留任何部分成交记录，认领标记释放后可重新清算。
func (e *Engine) SettleAuction(auctionID int64) (SettleResult, error) {
	e.mu.Lock()
	for {
		a, ok := e.auctions[auctionID]
		if !ok {
			e.mu.Unlock()
			return SettleResult{}, ErrAuctionNotFound
		}
		if s, done := e.settlements[auctionID]; done {
			e.mu.Unlock()
			return SettleResult{Outcome: SettleReplayed, Data: *s}, nil
		}
		if _, busy := e.settling[auctionID]; busy {
			e.cond.Wait()
			continue
		}
		now := e.clock.Now()
		if now.Before(a.Deadline) {
			e.mu.Unlock()
			return SettleResult{}, ErrAuctionNotClosed
		}

		// 阶段一：认领拍卖并固定同一份报价快照（锁内复制，锁外撮合）。
		e.settling[auctionID] = struct{}{}
		snapshot := make([]Bid, 0, len(e.bids[auctionID]))
		for _, b := range e.bids[auctionID] {
			snapshot = append(snapshot, *b)
		}
		available, reserve := a.AvailableQty, a.ReservePrice
		e.mu.Unlock()

		if e.testHook != nil {
			e.testHook(auctionID, snapshot)
		}

		// 阶段二：纯函数撮合，无锁、无副作用、无 I/O。
		settlement, err := settleFromSnapshot(available, reserve, snapshot, now)
		if err != nil {
			e.mu.Lock()
			delete(e.settling, auctionID)
			e.cond.Broadcast()
			e.mu.Unlock()
			return SettleResult{}, err
		}
		settlement.AuctionID = auctionID

		// 阶段三：回锁，以单一事件原子提交完整成交结果。
		e.mu.Lock()
		ev := Event{
			Type:       EvSettled,
			Timestamp:  now,
			AuctionID:  auctionID,
			Settlement: &settlement,
		}
		if _, err := e.store.Append([]Event{ev}); err != nil {
			delete(e.settling, auctionID)
			e.cond.Broadcast()
			e.mu.Unlock()
			return SettleResult{}, err
		}
		stored := settlement
		e.settlements[auctionID] = &stored
		a.Status = AuctionSettled
		delete(e.settling, auctionID)
		e.cond.Broadcast()
		e.mu.Unlock()
		return SettleResult{Outcome: SettleSettled, Data: stored}, nil
	}
}

// GetAuction 查询拍卖。
func (e *Engine) GetAuction(auctionID int64) (Auction, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := e.auctions[auctionID]
	if !ok {
		return Auction{}, ErrAuctionNotFound
	}
	return *a, nil
}

// GetBid 查询单份报价当前状态。
func (e *Engine) GetBid(auctionID int64, externalID string) (Bid, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.auctions[auctionID]; !ok {
		return Bid{}, ErrAuctionNotFound
	}
	b, ok := e.bids[auctionID][externalID]
	if !ok {
		return Bid{}, ErrBidNotFound
	}
	return *b, nil
}

// ListBids 列出一场拍卖的全部报价，按外部编号排序，顺序确定。
func (e *Engine) ListBids(auctionID int64) ([]Bid, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.auctions[auctionID]; !ok {
		return nil, ErrAuctionNotFound
	}
	out := make([]Bid, 0, len(e.bids[auctionID]))
	for _, b := range e.bids[auctionID] {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExternalID < out[j].ExternalID })
	return out, nil
}

// GetSettlement 查询一场拍卖的成交结果；未清算返回 ErrSettlementNotFound。
func (e *Engine) GetSettlement(auctionID int64) (Settlement, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.auctions[auctionID]; !ok {
		return Settlement{}, ErrAuctionNotFound
	}
	s, ok := e.settlements[auctionID]
	if !ok {
		return Settlement{}, ErrSettlementNotFound
	}
	return *s, nil
}

// GetFill 查询单份报价的成交明细；未成交返回 ErrFillNotFound。
func (e *Engine) GetFill(auctionID int64, externalID string) (Fill, error) {
	s, err := e.GetSettlement(auctionID)
	if err != nil {
		return Fill{}, err
	}
	for _, f := range s.Fills {
		if f.ExternalID == externalID {
			return f, nil
		}
	}
	return Fill{}, ErrFillNotFound
}
