package auctionclearing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Clock 是统一的当前时间来源。所有截止边界判断与时间戳都从它取值，
// 测试可注入可控时钟以精确复现“恰好截止/恰好清算”等边界情形。
type Clock interface {
	Now() time.Time
}

// SystemClock 使用操作系统墙上时间。
type SystemClock struct{}

// Now 返回当前系统时间（UTC）。
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// Service 是密封竞价拍卖的应用服务：创建、报价、撤回、清算、结果查询。
// 每个写用例都在单场拍卖的单个仓储事务内完成“判定 + 修改”，
// 因此幂等返回、冲突报错与并发清算互斥都由串行化天然保证。
type Service struct {
	repo        Repository
	clock       Clock
	compGateway CompensationGateway
}

// NewService 创建应用服务；clock 为 nil 时使用 SystemClock。
func NewService(repo Repository, clock Clock, opts ...ServiceOption) *Service {
	if clock == nil {
		clock = SystemClock{}
	}
	svc := &Service{repo: repo, clock: clock, compGateway: noopCompensationGateway{}}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// CreateAuction 创建一场拍卖：可售数量必须为正，保留价不可为负，截止点必填。
func (s *Service) CreateAuction(ctx context.Context, p CreateAuctionParams) (*Auction, error) {
	if p.AvailableQty <= 0 {
		return nil, NewError(KindInvalidArgument, "available quantity must be positive")
	}
	if p.ReservePrice.Cmp(ZeroMoney()) < 0 {
		return nil, NewError(KindInvalidArgument, "reserve price must not be negative")
	}
	if p.Deadline.IsZero() {
		return nil, NewError(KindInvalidArgument, "deadline is required")
	}
	now := s.clock.Now()
	a := &Auction{
		ID:           newAuctionID(),
		AvailableQty: p.AvailableQty,
		Deadline:     p.Deadline.UTC(),
		ReservePrice: p.ReservePrice,
		CreatedAt:    now,
		Status:       StatusOpen,
	}
	if err := s.repo.CreateAuction(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// GetAuction 查询拍卖本体及其状态。
func (s *Service) GetAuction(ctx context.Context, auctionID string) (*Auction, error) {
	if auctionID == "" {
		return nil, NewError(KindInvalidArgument, "auction id is required")
	}
	agg, err := s.repo.Load(ctx, auctionID)
	if err != nil {
		return nil, err
	}
	a := agg.Auction
	return &a, nil
}

// GetBid 查询单份报价（含其 active/withdrawn 状态）。
func (s *Service) GetBid(ctx context.Context, auctionID, externalRef string) (*Bid, error) {
	if auctionID == "" || externalRef == "" {
		return nil, NewError(KindInvalidArgument, "auction id and external ref are required")
	}
	agg, err := s.repo.Load(ctx, auctionID)
	if err != nil {
		return nil, err
	}
	b, ok := agg.Bids[externalRef]
	if !ok {
		return nil, fmt.Errorf("%w: bid %s", ErrNotFound, externalRef)
	}
	return b, nil
}

// PlaceBid 在截止前提交报价。
//   - 同一拍卖下同一外部报价号、内容完全相同：视为幂等重放，返回原报价且不
//     改动首次提交时间，Replayed=true（即使该报价后来被撤回，重放也不会恢复它）；
//   - 同一外部报价号但内容不一致：返回 KindConflict；
//   - 已截止或已清算后的“新”报价被拒绝；迟到报价绝不进入后续快照。
func (s *Service) PlaceBid(ctx context.Context, auctionID string, p PlaceBidParams) (*PlaceBidOutcome, error) {
	if auctionID == "" {
		return nil, NewError(KindInvalidArgument, "auction id is required")
	}
	if p.ExternalRef == "" {
		return nil, NewError(KindInvalidArgument, "external ref is required")
	}
	if p.Bidder == "" {
		return nil, NewError(KindInvalidArgument, "bidder is required")
	}
	if p.Quantity <= 0 {
		return nil, NewError(KindInvalidArgument, "bid quantity must be positive")
	}
	if !p.UnitPrice.Positive() {
		return nil, NewError(KindInvalidArgument, "unit price must be positive")
	}

	out := &PlaceBidOutcome{}
	_, err := s.repo.Update(ctx, auctionID, func(agg *Aggregate) error {
		if existing, ok := agg.Bids[p.ExternalRef]; ok {
			// 幂等判定先于一切状态判断：重放永远不改变状态，
			// 已撤回的报价不会因此复活，已清算后重试仍拿到原结果。
			if bidContentEqual(existing, p) {
				out.Bid = existing
				out.Replayed = true
				return nil
			}
			return ErrBidConflict
		}
		if agg.Auction.Status == StatusCleared {
			return ErrAuctionCleared
		}
		now := s.clock.Now()
		if !now.Before(agg.Auction.Deadline) {
			// now == deadline 即视为已截止：截止点是报价关闭的瞬间。
			return ErrBiddingClosed
		}
		bid := &Bid{
			AuctionID:        agg.Auction.ID,
			ExternalRef:      p.ExternalRef,
			Bidder:           p.Bidder,
			Quantity:         p.Quantity,
			UnitPrice:        p.UnitPrice,
			FirstSubmittedAt: now,
			UpdatedAt:        now,
			State:            BidActive,
		}
		agg.Bids[p.ExternalRef] = bid
		out.Bid = bid
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// WithdrawBid 在截止前撤回一份有效报价。
//   - 报价此前已撤回：幂等重放，返回现状，Replayed=true；
//   - 撤回与清算并发且清算先行提交：返回 KindInvalidState（ErrAuctionCleared），
//     撤回不会生效；而并发中先提交的撤回一定不在清算快照里——
//     两者共用同一把拍卖锁，结局唯一确定；
//   - 截止点之后不允许撤回。
func (s *Service) WithdrawBid(ctx context.Context, auctionID, externalRef string) (*WithdrawOutcome, error) {
	if auctionID == "" || externalRef == "" {
		return nil, NewError(KindInvalidArgument, "auction id and external ref are required")
	}

	out := &WithdrawOutcome{}
	_, err := s.repo.Update(ctx, auctionID, func(agg *Aggregate) error {
		bid, ok := agg.Bids[externalRef]
		if !ok {
			return fmt.Errorf("%w: bid %s", ErrNotFound, externalRef)
		}
		if bid.State == BidWithdrawn {
			out.Bid = bid
			out.Replayed = true
			return nil
		}
		if agg.Auction.Status == StatusCleared {
			return ErrAuctionCleared
		}
		now := s.clock.Now()
		if !now.Before(agg.Auction.Deadline) {
			return ErrBiddingClosed
		}
		bid.State = BidWithdrawn
		bid.WithdrawnAt = &now
		bid.UpdatedAt = now
		out.Bid = bid
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ClearAuction 对一场拍卖执行且仅执行一次成功清算。
//   - 必须已到截止点（now >= deadline），否则返回 ErrNotYetClosed；
//   - 清算在单事务内完成“取同一份快照 → 排序分配 → 写入结果与状态”，
//     提交要么整体成功要么整体放弃，绝不会遗留部分成交记录；
//   - 并发/重复调用：赢家之外的调用全部读到同一份原结果（Replayed=true）。
func (s *Service) ClearAuction(ctx context.Context, auctionID string) (*ClearOutcome, error) {
	if auctionID == "" {
		return nil, NewError(KindInvalidArgument, "auction id is required")
	}

	out := &ClearOutcome{}
	_, err := s.repo.Update(ctx, auctionID, func(agg *Aggregate) error {
		if agg.Clearing != nil {
			out.Result = agg.Clearing
			out.Replayed = true
			return nil
		}
		now := s.clock.Now()
		if now.Before(agg.Auction.Deadline) {
			return ErrNotYetClosed
		}
		// 快照与分配在事务内一次性完成；任何后续错误都只是“不提交”，
		// 聚合保持 open 且无成交数据，调用方可安全重试。
		snap := takeSnapshot(agg, now)
		result := clearFromSnapshot(agg.Auction, snap)
		agg.Clearing = result
		agg.Auction.Status = StatusCleared
		out.Result = result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetResult 查询已成功清算的成交结果；拍卖尚未清算时返回 KindNotFound。
func (s *Service) GetResult(ctx context.Context, auctionID string) (*ClearingResult, error) {
	if auctionID == "" {
		return nil, NewError(KindInvalidArgument, "auction id is required")
	}
	agg, err := s.repo.Load(ctx, auctionID)
	if err != nil {
		return nil, err
	}
	if agg.Clearing == nil {
		return nil, NewError(KindNotFound, "auction has no clearing result yet: "+auctionID)
	}
	return agg.Clearing, nil
}

// bidContentEqual 比较外部编号所绑定的报价内容是否一致（不含时间与状态）。
func bidContentEqual(b *Bid, p PlaceBidParams) bool {
	return b.Bidder == p.Bidder &&
		b.Quantity == p.Quantity &&
		b.UnitPrice.Cmp(p.UnitPrice) == 0
}

// newAuctionID 生成 128 位随机十六进制拍卖 ID。
func newAuctionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败属于运行环境严重故障，直接中断而非降级为弱随机。
		panic(fmt.Errorf("generate auction id: %w", err))
	}
	return hex.EncodeToString(b[:])
}
