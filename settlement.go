package auctionclearing

import "time"

// 结算与更正领域模型。
//
// 清算（Settlement in engine.go 中的成交结果）给出的是不可变的撮合事实；
// 结算流水则承载成交后的处理：成交确认 → 资金冻结 → 交割，以及结算后的更正。
// 结算按“版本”管理：清算后生成首版（版本号 1），每成功批准一次改变结算的
// 更正就追加一个新版本；只有最新且未被取代（Superseded=false）的版本是
// 当前有效成交结果，旧版本原样保留用于追溯。

// PriceVersion 是价格版本号：标识这批成交来自哪一次竞价撮合定价。
// 更正只改结算字段、不重新撮合，因此价格版本恒为清算时刻的 1；
// 更正申请必须记下它，价格版本变化即判定为冲突。
type PriceVersion int64

// SettlementVersion 是结算版本号：清算后生成结算时为 1；
// 每成功批准一次改变结算字段的更正递增 1（价格未变的更正不产生新版本）。
type SettlementVersion int64

const (
	// InitialPriceVersion 首版（也是唯一版）撮合价格版本号。
	InitialPriceVersion PriceVersion = 1
	// InitialSettlementVersion 清算后生成的首版结算版本号。
	InitialSettlementVersion SettlementVersion = 1
)

// SettlementEntryStatus 汇总一笔成交在当前结算版本上的处理进度，
// 由成交确认/资金冻结/交割三个步骤派生。
type SettlementEntryStatus string

const (
	// EntryPending 尚未完成成交确认。
	EntryPending SettlementEntryStatus = "pending"
	// EntryConfirmed 已成交确认但资金尚未冻结。
	EntryConfirmed SettlementEntryStatus = "confirmed"
	// EntryFrozen 资金已冻结但尚未交割（更正可直接调整未交割部分结算）。
	EntryFrozen SettlementEntryStatus = "frozen"
	// EntryDelivered 已全部交割（更正只能以补偿单补差，原结算不可变）。
	EntryDelivered SettlementEntryStatus = "delivered"
)

// SettlementRecord 是一批成交在某个结算版本上的完整结算快照。
type SettlementRecord struct {
	AuctionID    int64             `json:"auction_id"`
	PriceVersion PriceVersion      `json:"price_version"`
	Version      SettlementVersion `json:"version"`
	CreatedAt    time.Time         `json:"created_at"`
	Superseded   bool              `json:"superseded"`
	Entries      []SettlementEntry `json:"entries"`
	TotalSettled Money             `json:"total_settled"`
}

// SettlementEntry 一笔成交在某个结算版本上的结算条目。
//
// 字段分两类，更正只能动“结算字段”：
//   - 原始撮合信息（ExternalID/Bidder/Rank/MatchedQty/OriginalUnitPrice、
//     价格版本）在任何版本中都按首版原样保留，更正不改撮合、不重新定价数量；
//   - 交割状态（Confirmed/FundsFrozen/DeliveredQty/DeliveredAmount/
//     DeliveredAt）按原版本继承：已经交割的事实不能被更正抹掉；
//   - 可更正的结算字段只有 EffectiveUnitPrice（结算单价）与
//     SettledAmount（结算金额）。
type SettlementEntry struct {
	ExternalID string `json:"external_id"`
	Bidder     string `json:"bidder"`
	Rank       int    `json:"rank"`

	// 原始撮合信息（不可变，任何更正版本都保留原值）。
	MatchedQty        int64        `json:"matched_qty"`
	OriginalUnitPrice Money        `json:"original_unit_price"`
	PriceVersion      PriceVersion `json:"price_version"`

	// 可更正的结算字段：仅这两项允许被更正改变。
	EffectiveUnitPrice Money `json:"effective_unit_price"`
	SettledAmount      Money `json:"settled_amount"`

	// 成交确认 / 资金冻结 / 交割。已交割事实不可撤销：
	// DeliveredQty/DeliveredAmount 只增不减，在新版本中原样继承。
	Confirmed       bool       `json:"confirmed"`
	FundsFrozen     bool       `json:"funds_frozen"`
	DeliveredQty    int64      `json:"delivered_qty"`
	DeliveredAmount Money      `json:"delivered_amount"`
	DeliveredAt     *time.Time `json:"delivered_at,omitempty"`
}

// UndeliveredQty 尚未交割的数量；更正只允许对这部分直接调整结算单价。
func (e SettlementEntry) UndeliveredQty() int64 { return e.MatchedQty - e.DeliveredQty }

// Status 派生当前条目所处的结算阶段。
func (e SettlementEntry) Status() SettlementEntryStatus {
	switch {
	case e.DeliveredQty >= e.MatchedQty:
		return EntryDelivered
	case e.FundsFrozen:
		return EntryFrozen
	case e.Confirmed:
		return EntryConfirmed
	default:
		return EntryPending
	}
}

// newSettlementRecordFromClearing 以不可变的清算成交结果生成首版结算：
// 成交按统一清算价结算，成交确认/资金冻结/交割均尚未发生。
func newSettlementRecordFromClearing(st Settlement, now time.Time) *SettlementRecord {
	rec := &SettlementRecord{
		AuctionID:    st.AuctionID,
		PriceVersion: InitialPriceVersion,
		Version:      InitialSettlementVersion,
		CreatedAt:    now,
		Entries:      make([]SettlementEntry, 0, len(st.Fills)),
	}
	for _, f := range st.Fills {
		amount, err := f.Price.Mul(f.FilledQty)
		if err != nil {
			// 清算结果金额在撮合阶段已校验过可乘可加，这里不应溢出。
			panic(err)
		}
		rec.Entries = append(rec.Entries, SettlementEntry{
			ExternalID:         f.ExternalID,
			Bidder:             f.Bidder,
			Rank:               f.Rank,
			MatchedQty:         f.FilledQty,
			OriginalUnitPrice:  f.Price,
			PriceVersion:       InitialPriceVersion,
			EffectiveUnitPrice: f.Price,
			SettledAmount:      amount,
		})
		total, err := rec.TotalSettled.Add(amount)
		if err != nil {
			panic(err)
		}
		rec.TotalSettled = total
	}
	return rec
}

// CorrectionStatus 更正申请的处理状态。
type CorrectionStatus string

const (
	// CorrectionSubmitted 申请已登记，尚未批准。
	CorrectionSubmitted CorrectionStatus = "submitted"
	// CorrectionCompleted 批准完成：更正已全部生效（含无变化留痕）。
	CorrectionCompleted CorrectionStatus = "completed"
	// CorrectionPending 批准时存在补偿单尚未结清（典型：补偿结算失败），
	// 原成交仍可查询、更正停在待处理，绝不伪装成已完成。
	CorrectionPending CorrectionStatus = "pending"
)

// CompensationStatus 补偿单状态。
type CompensationStatus string

const (
	// CompensationPending 待补偿：外部补偿结算尚未成功。
	CompensationPending CompensationStatus = "pending"
	// CompensationSettled 补偿已成功结清。
	CompensationSettled CompensationStatus = "settled"
)

// CorrectionItemSnapshot 是申请登记时冻结的单笔成交快照：
// 成交清单、价格版本与当时结算状态一并保存，作为日后比对与追溯依据。
type CorrectionItemSnapshot struct {
	ExternalID      string                `json:"external_id"`
	Rank            int                   `json:"rank"`
	MatchedQty      int64                 `json:"matched_qty"`
	DeliveredQty    int64                 `json:"delivered_qty"`
	OriginalPrice   Money                 `json:"original_price"`
	PriceVersion    PriceVersion          `json:"price_version"`
	SettlementState SettlementEntryStatus `json:"settlement_state"`
}

// CorrectionItem 一张更正申请中的单笔成交更正指令（只允许指定目标结算单价）。
type CorrectionItem struct {
	ExternalID      string `json:"external_id"`
	TargetUnitPrice Money  `json:"target_unit_price"`
}

// Compensation 针对一笔已交割成交（或其已交割部分）生成的补偿单。
// 原成交不动，差额在此单独走补偿结算；补偿结算失败时保持 pending。
type Compensation struct {
	ExternalID string `json:"external_id"`
	Rank       int    `json:"rank"`
	// Quantity 被补偿的已交割数量。
	Quantity int64 `json:"quantity"`
	// LockedAmount 交割时已锁定的结算总额（交割可能跨版本发生，
	// 因此记录总额而非单价，避免除法丢精度）。
	LockedAmount Money `json:"locked_amount"`
	// TargetPrice 更正目标单价。
	TargetPrice Money `json:"target_price"`
	// Amount 有符号补偿差额：LockedAmount - TargetPrice*Quantity，精确计算；
	// 为正表示向成交方退回多收，为负表示补收。
	Amount      Money              `json:"amount"`
	Status      CompensationStatus `json:"status"`
	CreatedAt   time.Time          `json:"created_at"`
	SettledAt   *time.Time         `json:"settled_at,omitempty"`
	FailureNote string             `json:"failure_note,omitempty"`
}

// CorrectionEvent 记录一次批准动作的审计痕迹：
// 即便更正没有改变成交价格，也必须记录依据与处理人，而不是仅返回“无变化”。
type CorrectionEvent struct {
	At          time.Time         `json:"at"`
	Handler     string            `json:"handler"`
	Basis       string            `json:"basis"`
	Changed     bool              `json:"changed"`
	FromVersion SettlementVersion `json:"from_version"`
	ToVersion   SettlementVersion `json:"to_version"` // 与 FromVersion 相同表示未产生新版本
	Note        string            `json:"note,omitempty"`
}

// CorrectionApplication 一张成交更正申请及其完整处理结果。
type CorrectionApplication struct {
	CorrectionID      string                   `json:"correction_id"`
	AuctionID         int64                    `json:"auction_id"`
	BatchID           int64                    `json:"batch_id"` // 竞价批次，即拍卖 ID
	Reason            string                   `json:"reason"`
	Basis             string                   `json:"basis"`
	Handler           string                   `json:"handler"`
	PriceVersion      PriceVersion             `json:"price_version"`
	BaseSettleVersion SettlementVersion        `json:"base_settlement_version"`
	Items             []CorrectionItem         `json:"items"`
	Snapshots         []CorrectionItemSnapshot `json:"snapshots"`
	Status            CorrectionStatus         `json:"status"`
	SubmittedAt       time.Time                `json:"submitted_at"`
	ApprovedAt        *time.Time               `json:"approved_at,omitempty"`
	EffectiveVersion  SettlementVersion        `json:"effective_version,omitempty"`
	Compensations     []Compensation           `json:"compensations,omitempty"`
	Events            []CorrectionEvent        `json:"events,omitempty"`
}
