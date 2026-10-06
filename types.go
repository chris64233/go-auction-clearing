package auctionclearing

import "time"

type AuctionStatus string

const (
	AuctionOpen    AuctionStatus = "OPEN"
	AuctionCleared AuctionStatus = "CLEARED"
)

// Auction 是一场竞价，SettlementVersion 是结算版本号：
// 每次成交更正都会使其递增，交割确认与批次关闭都以它为裁决依据。
type Auction struct {
	ID                string
	Status            AuctionStatus
	SettlementVersion int64
}

type TradeStatus string

const (
	TradeActive    TradeStatus = "ACTIVE"
	TradeCancelled TradeStatus = "CANCELLED"
)

// Trade 是一笔成交。DeliveredQuantity 记录累计已交割数量。
type Trade struct {
	ID                string
	AuctionID         string
	BuyerID           string
	SellerID          string
	Quantity          int64
	PriceCents        int64
	DeliveredQuantity int64
	Status            TradeStatus
	CreatedVersion    int64
}

// Correction 是一次成交更正，生效后拍卖的结算版本递增。
type Correction struct {
	ID            string
	TradeID       string
	Version       int64
	OldQuantity   int64
	NewQuantity   int64
	OldPriceCents int64
	NewPriceCents int64
	Reason        string
	CreatedAt     time.Time
}

type BatchStatus string

const (
	BatchOpen   BatchStatus = "OPEN"
	BatchClosed BatchStatus = "CLOSED"
)

type ItemStatus string

const (
	ItemPending   ItemStatus = "PENDING"
	ItemPartial   ItemStatus = "PARTIAL"
	ItemCompleted ItemStatus = "COMPLETED"
	ItemCancelled ItemStatus = "CANCELLED"
)

// BatchItem 是批次中冻结的一笔成交：数量与价格在建批时固定，
// 之后的成交更正不会改写已冻结内容，只会让批次过期。
type BatchItem struct {
	TradeID           string
	PlannedQuantity   int64
	DeliveredQuantity int64
	PriceCents        int64
	Status            ItemStatus
}

// DeliveryBatch 冻结同一场拍卖的一组成交及其结算版本与截止时间。
type DeliveryBatch struct {
	ID        string
	AuctionID string
	Version   int64
	Deadline  time.Time
	Status    BatchStatus
	Items     []*BatchItem
	CreatedAt time.Time
	ClosedAt  *time.Time
}

type MarginAction string

const (
	MarginReleased           MarginAction = "RELEASED"
	MarginForfeited          MarginAction = "FORFEITED"
	MarginPartiallyForfeited MarginAction = "PARTIALLY_FORFEITED"
)

// MarginResult 是单笔交割确认/取消对应的保证金处理结果。
type MarginResult struct {
	Action      MarginAction
	AmountCents int64
}

// MarginRecord 是保证金台账记录，只随交割确认或取消原子产生，
// 不会出现没有对应交割记录的孤立保证金记录。
type MarginRecord struct {
	ID        string
	OpID      string
	BatchID   string
	TradeID   string
	Version   int64
	Result    MarginResult
	CreatedAt time.Time
}

// DeliveryConfirmation 是一笔成交的交割确认，OpID 是外部操作号，
// 用于幂等重放。
type DeliveryConfirmation struct {
	OpID      string
	BatchID   string
	TradeID   string
	Quantity  int64
	Margin    MarginResult
	Version   int64
	CreatedAt time.Time
}

// DeliveryCancellation 明确取消批次内一笔成交的剩余未交割数量。
type DeliveryCancellation struct {
	OpID              string
	BatchID           string
	TradeID           string
	CancelledQuantity int64
	Margin            MarginResult
	Version           int64
	Reason            string
	CreatedAt         time.Time
}
