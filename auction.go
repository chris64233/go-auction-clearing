package auctionclearing

import (
	"context"
	"time"
)

// AuctionStatus 拍卖生命周期状态。
type AuctionStatus string

const (
	// StatusOpen 报价中（截止点之前或已截止但尚未清算）。
	StatusOpen AuctionStatus = "open"
	// StatusCleared 已成功清算（一场拍卖至多进入一次该状态）。
	StatusCleared AuctionStatus = "cleared"
)

// BidState 报价生命周期状态。
type BidState string

const (
	// BidActive 有效报价。
	BidActive BidState = "active"
	// BidWithdrawn 已撤回，清算快照中不得出现。
	BidWithdrawn BidState = "withdrawn"
)

// Auction 记录一场多单位密封竞价拍卖的静态参数与状态。
type Auction struct {
	ID           string        `json:"id"`
	AvailableQty int64         `json:"available_qty"` // 可售数量（>0）
	Deadline     time.Time     `json:"deadline"`      // 报价截止点
	ReservePrice Money         `json:"reserve_price"` // 最低成交价（保留价，低于此价的报价不参与分配）
	CreatedAt    time.Time     `json:"created_at"`
	Status       AuctionStatus `json:"status"`
}

// Bid 一份密封报价。外部编号在同一场拍卖内唯一，内容（竞买方/数量/单价）
// 首次提交后不可变；时间字段记录“首次有效提交”，用于同价排序。
type Bid struct {
	AuctionID string `json:"auction_id"`
	// ExternalRef 外部报价号，由调用方提供，幂等键。
	ExternalRef string `json:"external_ref"`
	Bidder      string `json:"bidder"`
	Quantity    int64  `json:"quantity"`
	UnitPrice   Money  `json:"unit_price"`
	// FirstSubmittedAt 首次有效提交时间，同价报价按它升序分配。
	FirstSubmittedAt time.Time `json:"first_submitted_at"`
	// UpdatedAt 最近一次幂等重放提交的时间；内容永不改变。
	UpdatedAt   time.Time  `json:"updated_at"`
	State       BidState   `json:"state"`
	WithdrawnAt *time.Time `json:"withdrawn_at,omitempty"`
}

// Fill 一笔成交（边界报价可能只部分成交）。单价歧视规则：按报价自身单价成交。
type Fill struct {
	ExternalRef string `json:"external_ref"`
	Bidder      string `json:"bidder"`
	Quantity    int64  `json:"quantity"`
	UnitPrice   Money  `json:"unit_price"`
	Amount      Money  `json:"amount"` // UnitPrice * Quantity，精确金额
	// Seq 分配顺序（从 1 开始），便于追溯边界报价。
	Seq int `json:"seq"`
}

// ClearingResult 一次成功清算的不可变结果。
type ClearingResult struct {
	AuctionID    string    `json:"auction_id"`
	ClearedAt    time.Time `json:"cleared_at"`
	SnapshotAt   time.Time `json:"snapshot_at"` // 快照取值时的统一时间
	AvailableQty int64     `json:"available_qty"`
	SoldQty      int64     `json:"sold_qty"`
	ReservePrice Money     `json:"reserve_price"`
	// BidCount 进入快照的有效报价数（用于审计，不随成交与否变化）。
	BidCount      int    `json:"bid_count"`
	TotalProceeds Money  `json:"total_proceeds"`
	Fills         []Fill `json:"fills"`
}

// CreateAuctionParams 创建拍卖的入参。
type CreateAuctionParams struct {
	AvailableQty int64     `json:"available_qty"`
	Deadline     time.Time `json:"deadline"`
	ReservePrice Money     `json:"reserve_price"`
}

// PlaceBidParams 提交报价的入参。
type PlaceBidParams struct {
	ExternalRef string `json:"external_ref"`
	Bidder      string `json:"bidder"`
	Quantity    int64  `json:"quantity"`
	UnitPrice   Money  `json:"unit_price"`
}

// PlaceBidOutcome 报价结果；Replayed=true 表示命中同一外部编号的幂等重放。
type PlaceBidOutcome struct {
	Bid      *Bid `json:"bid"`
	Replayed bool `json:"replayed"`
}

// WithdrawOutcome 撤回结果；Replayed=true 表示此前已经撤回。
type WithdrawOutcome struct {
	Bid      *Bid `json:"bid"`
	Replayed bool `json:"replayed"`
}

// ClearOutcome 清算结果；Replayed=true 表示拍卖此前已清算，返回的是原结果。
type ClearOutcome struct {
	Result   *ClearingResult `json:"result"`
	Replayed bool            `json:"replayed"`
}

// Aggregate 一场拍卖的完整持久化聚合：拍卖、报价集合、清算结果同生共死，
// 保证清算写入是原子的（不会出现状态已改但结果缺失等部分成交残留）。
type Aggregate struct {
	Auction  Auction         `json:"auction"`
	Bids     map[string]*Bid `json:"bids"`
	Clearing *ClearingResult `json:"clearing,omitempty"`
}

// Repository 聚合仓储。所有针对单场拍卖的读写都经由 Update/Load 串行化，
// 实现可以是纯内存或落盘（JSON 原子替换）。
type Repository interface {
	// CreateAuction 写入新拍卖；ID 冲突返回 KindConflict 错误。
	CreateAuction(ctx context.Context, a *Auction) error
	// Update 在单个事务内读取并修改一场拍卖的聚合。
	// fn 对聚合的修改仅在返回 nil 时提交；返回错误则整体放弃。
	// 提交后返回聚合的深拷贝，可在锁外安全读取。
	Update(ctx context.Context, auctionID string, fn func(agg *Aggregate) error) (*Aggregate, error)
	// Load 只读加载一场拍卖的聚合深拷贝。
	Load(ctx context.Context, auctionID string) (*Aggregate, error)
}
