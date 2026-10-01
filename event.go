package auctionclearing

import "time"

// EventType 是拍卖领域事件的类型。
type EventType string

const (
	// EvAuctionCreated 表示一场拍卖被创建。
	EvAuctionCreated EventType = "auction_created"
	// EvBidSubmitted 表示一份新报价被接受。
	EvBidSubmitted EventType = "bid_submitted"
	// EvBidWithdrawn 表示一份有效报价被撤回。
	EvBidWithdrawn EventType = "bid_withdrawn"
	// EvSettled 表示一场拍卖原子清算完成，事件内携带完整成交结果。
	EvSettled EventType = "settled"
	// EvSettlementGenerated 表示清算后生成首版结算（成交确认/冻结/交割的前提）。
	EvSettlementGenerated EventType = "settlement_generated"
	// EvFillConfirmed 表示一笔成交完成成交确认。
	EvFillConfirmed EventType = "fill_confirmed"
	// EvFundsFrozen 表示一笔成交的资金已冻结。
	EvFundsFrozen EventType = "funds_frozen"
	// EvFillDelivered 表示一笔成交发生增量交割，事件携带本次交割量与锁定金额。
	EvFillDelivered EventType = "fill_delivered"
	// EvCorrectionSubmitted 表示一张成交更正申请被登记（含成交清单快照）。
	EvCorrectionSubmitted EventType = "correction_submitted"
	// EvCorrectionApproved 表示一次更正批准动作落定：
	// 价格变化且补偿全部成功时，事件内携带完整新结算版本（与申请原子提交，
	// 存储层不可能出现“申请已完成但新版本缺失”）；补偿失败停留 pending、
	// 或价格未变留痕时，NewSettlement 为空。
	EvCorrectionApproved EventType = "correction_approved"
)

// Event 是持久化的最小事实单元。
// 一次清算只产生一个 EvSettled 事件（内含全部 fills），
// 因此“成交结果”在存储层天然原子：要么整笔可见，要么完全不存在。
type Event struct {
	// Seq 是全局单调递增序号，由 EventStore 在一次原子追加中分配。
	Seq int64 `json:"seq"`
	// Type 是事件类型。
	Type EventType `json:"type"`
	// Timestamp 是事件发生时间（取自统一 Clock）。
	Timestamp time.Time `json:"timestamp"`
	// AuctionID 是所属拍卖。
	AuctionID int64 `json:"auction_id"`

	// —— EvAuctionCreated ——
	AvailableQty int64      `json:"available_qty,omitempty"`
	Deadline     *time.Time `json:"deadline,omitempty"`
	ReservePrice Money      `json:"reserve_price,omitempty"`

	// —— EvBidSubmitted / EvBidWithdrawn ——
	ExternalID string `json:"external_id,omitempty"`
	Bidder     string `json:"bidder,omitempty"`
	Qty        int64  `json:"qty,omitempty"`
	Price      Money  `json:"price,omitempty"`

	// —— EvSettled（完整结果，单一事件原子落盘）——
	Settlement *Settlement `json:"settlement,omitempty"`

	// —— 结算流水 / 成交更正 ——
	// DeliveryQty 是 EvFillDelivered 的本次增量交割数量。
	DeliveryQty int64 `json:"delivery_qty,omitempty"`
	// DeliveryAmount 是本次交割按当时有效单价锁定的金额。
	DeliveryAmount Money `json:"delivery_amount,omitempty"`
	// SettlementVersion 是确认/冻结/交割事件作用的结算版本：
	// 重放时只施加到该版本条目，绝不叠加到后续更正产生的新版本上。
	SettlementVersion SettlementVersion `json:"settlement_version,omitempty"`
	// SettlementRecord 为 EvSettlementGenerated 的首版结算、
	// EvCorrectionApproved 价格变化时的新版本（nil 表示未产生新版本）。
	SettlementRecord *SettlementRecord `json:"settlement_record,omitempty"`
	// Correction 为更正申请事件携带的完整申请（含快照/补偿/审计痕迹）。
	Correction *CorrectionApplication `json:"correction,omitempty"`
}
