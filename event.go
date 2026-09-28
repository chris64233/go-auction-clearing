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
}
