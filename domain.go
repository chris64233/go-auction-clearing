package auctionclearing

import "time"

// AuctionStatus 表示拍卖生命周期状态。
type AuctionStatus string

const (
	// AuctionOpen 表示报价窗口开放中，允许报价与撤回。
	AuctionOpen AuctionStatus = "open"
	// AuctionSettled 表示拍卖已成功清算，结果不可变。
	AuctionSettled AuctionStatus = "settled"
)

// Auction 记录一场多单位密封竞价拍卖。
type Auction struct {
	// ID 是拍卖内部主键（由仓储分配，单调递增）。
	ID int64
	// AvailableQty 是可售数量（> 0）。
	AvailableQty int64
	// Deadline 是报价截止点；Now == Deadline 即视为已截止（左闭右开）。
	Deadline time.Time
	// ReservePrice 是最低成交价：低于该价的报价一律不成交。
	ReservePrice Money
	// Status 为 open 或 settled。
	Status AuctionStatus
	// CreatedAt 为创建时间。
	CreatedAt time.Time
}

// BidStatus 表示报价在快照中的生命周期状态。
type BidStatus string

const (
	// BidActive 表示报价当前有效。
	BidActive BidStatus = "active"
	// BidWithdrawn 表示报价已被撤回。
	BidWithdrawn BidStatus = "withdrawn"
)

// Bid 是一份密封报价。
type Bid struct {
	// AuctionID 所属拍卖。
	AuctionID int64
	// ExternalID 是外部报价编号，在同一场拍卖内唯一，是幂等与冲突判定的键。
	ExternalID string
	// Bidder 是竞买方标识。
	Bidder string
	// Qty 是竞买数量（> 0）。
	Qty int64
	// Price 是报价单价。
	Price Money
	// SubmittedAt 是首次有效提交时间（用于同价排序）。
	SubmittedAt time.Time
	// UpdatedAt 是最近一次成功操作（提交/撤回）的时间。
	UpdatedAt time.Time
	// Status 为 active 或 withdrawn。
	Status BidStatus
}

// BidInput 是报价提交参数。
type BidInput struct {
	AuctionID  int64
	ExternalID string
	Bidder     string
	Qty        int64
	Price      Money
}

// Fill 表示一份报价在清算中的成交结果。
type Fill struct {
	// ExternalID 对应成交报价的外部编号。
	ExternalID string
	// Bidder 是成交竞买方。
	Bidder string
	// Price 是成交单价：密封统一价格清算，等于边界报价单价。
	Price Money
	// RequestedQty 是该报价原始申报数量。
	RequestedQty int64
	// FilledQty 是实际成交数量（边界报价可部分成交，其余报价要么全成要么不成）。
	FilledQty int64
	// Rank 是分配顺序（从 1 开始；按单价降序、同价按首次提交先后）。
	Rank int
}

// Settlement 是一场拍卖的清算结果。
type Settlement struct {
	// AuctionID 对应拍卖。
	AuctionID int64
	// ClearingPrice 是统一成交价；无任何成交时为 0。
	ClearingPrice Money
	// SoldQty 是总成交数量，不超过可售量。
	SoldQty int64
	// TotalProceeds 是成交总金额（各 Fill 的 FilledQty*Price 之和）。
	TotalProceeds Money
	// Fills 按分配顺序排列的成交明细。
	Fills []Fill
	// SettledAt 是清算完成时间。
	SettledAt time.Time
}

// BidOutcome 表示一次报价提交请求的处理结果。
type BidOutcome string

const (
	// BidAccepted 表示新报价被接受。
	BidAccepted BidOutcome = "accepted"
	// BidReplayed 表示同一外部编号、相同内容的重复请求，返回原有记录。
	BidReplayed BidOutcome = "replayed"
)

// BidResult 是提交报价的返回值。
type BidResult struct {
	Outcome BidOutcome
	Bid     Bid
}

// WithdrawOutcome 表示一次撤回请求的处理结果。
type WithdrawOutcome string

const (
	// WithdrawDone 表示本次请求实际执行了撤回。
	WithdrawDone WithdrawOutcome = "withdrawn"
	// WithdrawReplayed 表示该编号此前已被撤回，重复请求返回原状态。
	WithdrawReplayed WithdrawOutcome = "replayed"
)

// WithdrawResult 是撤回报价的返回值。
type WithdrawResult struct {
	Outcome WithdrawOutcome
	Bid     Bid
}

// SettleOutcome 表示一次清算请求的处理结果。
type SettleOutcome string

const (
	// SettleSettled 表示本次请求实际完成了清算。
	SettleSettled SettleOutcome = "settled"
	// SettleReplayed 表示拍卖此前已清算，返回原有结果。
	SettleReplayed SettleOutcome = "replayed"
)

// SettleResult 是清算请求的返回值。
type SettleResult struct {
	Outcome SettleOutcome
	Data    Settlement
}
