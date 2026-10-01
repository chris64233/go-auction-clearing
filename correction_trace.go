package auctionclearing

import "time"

// TraceScenario 标识一笔成交更正在批准时落入的处理情形。
type TraceScenario string

const (
	// ScenarioUndelivered 未交割：直接在新结算版本上改价，无补偿单。
	ScenarioUndelivered TraceScenario = "undelivered"
	// ScenarioDelivered 已交割：原成交不动，全额以补偿单补差。
	ScenarioDelivered TraceScenario = "delivered_compensated"
	// ScenarioPartial 部分补偿：部分已交割（补偿单补差）+ 部分未交割（直接改价）。
	ScenarioPartial TraceScenario = "partial_compensation"
	// ScenarioSubmitted 申请已登记但尚未批准，链路停在申请快照。
	ScenarioSubmitted TraceScenario = "submitted"
)

// OriginalFillView 原成交（撮合）信息，始终取自不可变的清算结果。
type OriginalFillView struct {
	ExternalID string `json:"external_id"`
	Bidder     string `json:"bidder"`
	Rank       int    `json:"rank"`
	Price      Money  `json:"price"`
	Quantity   int64  `json:"quantity"`
	Amount     Money  `json:"amount"`
}

// CorrectionTraceItem 一笔成交更正的完整追溯：
// 原成交 → 申请快照 → 补偿单（如有）→ 最终结算条目。
type CorrectionTraceItem struct {
	ExternalID      string                 `json:"external_id"`
	Scenario        TraceScenario          `json:"scenario"`
	TargetUnitPrice Money                  `json:"target_unit_price"`
	Original        OriginalFillView       `json:"original"`
	SubmitSnapshot  CorrectionItemSnapshot `json:"submit_snapshot"`
	Compensation    *Compensation          `json:"compensation,omitempty"`
	// FinalSettlement 是更正生效版本上的条目；价格未变/失败时取当前有效版本。
	FinalSettlement *SettlementEntry `json:"final_settlement"`
	// CurrentSettlement 仅当生效版本已被后续更正取代时出现，指向最新版本。
	CurrentSettlement *SettlementEntry `json:"current_settlement,omitempty"`
}

// CorrectionTrace 一张更正申请的端到端追溯视图：
// 从更正号一路追到原成交、申请原因/依据/处理人、补偿单与最终结算。
type CorrectionTrace struct {
	CorrectionID     string                `json:"correction_id"`
	BatchID          int64                 `json:"batch_id"`
	Reason           string                `json:"reason"`
	Basis            string                `json:"basis"`
	Handler          string                `json:"handler"`
	PriceVersion     PriceVersion          `json:"price_version"`
	BaseVersion      SettlementVersion     `json:"base_settlement_version"`
	EffectiveVersion SettlementVersion     `json:"effective_version"`
	CurrentVersion   SettlementVersion     `json:"current_settlement_version"`
	Status           CorrectionStatus      `json:"status"`
	SubmittedAt      time.Time             `json:"submitted_at"`
	ApprovedAt       *time.Time            `json:"approved_at,omitempty"`
	Events           []CorrectionEvent     `json:"events"`
	Items            []CorrectionTraceItem `json:"items"`
}
