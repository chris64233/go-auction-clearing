package auctionclearing

import "time"

// TradeStatus 成交生命周期状态。
type TradeStatus string

const (
	TradeStatusPending    TradeStatus = "PENDING"    // 已生成,待清算
	TradeStatusCleared    TradeStatus = "CLEARED"    // 清算完成,可加入交割批次
	TradeStatusDelivering TradeStatus = "DELIVERING" // 已冻结在未完成批次中
	TradeStatusDelivered  TradeStatus = "DELIVERED"  // 全部交割完成
	TradeStatusCancelled  TradeStatus = "CANCELLED"  // 已取消
)

// Trade 一笔成交。Quantity/Price 可被更正,每次更正 SettlementVersion 递增。
type Trade struct {
	ID                string
	AuctionID         string
	BuyerID           string
	SellerID          string
	Quantity          int64
	Price             int64
	Status            TradeStatus
	SettlementVersion int64
	CorrectionOf      string // 若非空,表示本成交是对哪笔成交的更正关联
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// BatchStatus 交割批次状态。
type BatchStatus string

const (
	BatchStatusOpen   BatchStatus = "OPEN"
	BatchStatusClosed BatchStatus = "CLOSED"
)

// DeliveryBatch 交割批次,冻结成交清单、数量、价格版本与截止时间。
type DeliveryBatch struct {
	ID        string
	AuctionID string
	Status    BatchStatus
	Items     []*BatchItem
	Deadline  time.Time
	CreatedAt time.Time
	ClosedAt  time.Time
}

// ItemStatus 批次内成交条目状态。
type ItemStatus string

const (
	ItemStatusOpen      ItemStatus = "OPEN"
	ItemStatusCompleted ItemStatus = "COMPLETED"
	ItemStatusCancelled ItemStatus = "CANCELLED"
)

// BatchItem 批次内的一笔成交冻结快照。
type BatchItem struct {
	TradeID           string
	PlannedQuantity   int64 // 冻结时的成交数量
	DeliveredQuantity int64 // 已累计实际交割数量
	Price             int64 // 冻结时的价格
	SettlementVersion int64 // 冻结时的结算版本
	Status            ItemStatus
}

// RemainingQuantity 剩余待交割数量。
func (i *BatchItem) RemainingQuantity() int64 {
	return i.PlannedQuantity - i.DeliveredQuantity
}

// MarginAction 保证金处理动作。
type MarginAction string

const (
	MarginActionRelease MarginAction = "RELEASE" // 释放已交割部分保证金
	MarginActionForfeit MarginAction = "FORFEIT" // 违约没收
	MarginActionNone    MarginAction = "NONE"
)

// MarginRecord 一次交割确认产生的保证金处理结果。
type MarginRecord struct {
	TradeID string
	BatchID string
	Action  MarginAction
	Amount  int64
	OpID    string
}

// DeliveryConfirmation 一笔交割确认记录。
type DeliveryConfirmation struct {
	ID        string
	OpID      string // 外部操作号,幂等键
	BatchID   string
	TradeID   string
	Quantity  int64 // 本次实际交割数量
	Margin    MarginRecord
	Version   int64 // 确认时依据的结算版本
	CreatedAt time.Time
}

// BatchItemView 查询视角:批次内单笔成交的计划/已交割/剩余与保证金。
type BatchItemView struct {
	TradeID           string
	Status            ItemStatus
	PlannedQuantity   int64
	DeliveredQuantity int64
	RemainingQuantity int64
	Price             int64
	SettlementVersion int64
	CurrentVersion    int64 // 成交当前结算版本(用于发现更正)
	CorrectionOf      string
	Margins           []MarginRecord
}

// BatchView 批次查询视图。
type BatchView struct {
	BatchID   string
	AuctionID string
	Status    BatchStatus
	Deadline  time.Time
	Items     []BatchItemView
}
