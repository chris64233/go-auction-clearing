package auctionclearing

import (
	"context"
	"fmt"
	"time"
)

// TraceScenario 标识一笔成交更正在批准时落入的处理情形。
type TraceScenario string

const (
	// ScenarioUndelivered 未交割：直接在新结算版本上改价，无补偿单。
	ScenarioUndelivered TraceScenario = "undelivered"
	// ScenarioDelivered 已交割：原成交不动，全额以补偿单补差。
	ScenarioDelivered TraceScenario = "delivered_compensated"
	// ScenarioPartial 部分补偿：部分已交割（补偿单补差）+ 部分未交割（直接改价）。
	ScenarioPartial TraceScenario = "partial_compensation"
)

// OriginalFillView 原成交（撮合）信息，始终取自不可变的清算结果。
type OriginalFillView struct {
	ExternalRef string `json:"external_ref"`
	Bidder      string `json:"bidder"`
	Seq         int    `json:"seq"`
	Quantity    int64  `json:"quantity"`
	UnitPrice   Money  `json:"unit_price"`
	Amount      Money  `json:"amount"`
}

// CorrectionTraceItem 一笔成交更正的完整追溯：原成交 → 申请快照 →
// 补偿单（如有）→ 最终结算条目。
type CorrectionTraceItem struct {
	ExternalRef       string                 `json:"external_ref"`
	Scenario          TraceScenario          `json:"scenario"`
	TargetUnitPrice   Money                  `json:"target_unit_price"`
	Original          OriginalFillView       `json:"original"`
	SubmitSnapshot    CorrectionItemSnapshot `json:"submit_snapshot"`
	Compensation      *Compensation          `json:"compensation,omitempty"`
	FinalSettlement   *SettlementEntry       `json:"final_settlement"`
	CurrentSettlement *SettlementEntry       `json:"current_settlement,omitempty"`
}

// CorrectionTrace 一张更正申请的端到端追溯视图。
type CorrectionTrace struct {
	CorrectionID     string                `json:"correction_id"`
	BatchID          string                `json:"batch_id"`
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

// GetCorrectionTrace 组装一张更正的完整链路：从更正号追到原成交、申请原因、
// 补偿单与最终结算。已交割 / 未交割 / 部分补偿三种情形的条目结构不同：
//   - 未交割：Compensation 为空，FinalSettlement 直接体现新单价；
//   - 已交割：Compensation 为全额差额补偿单，FinalSettlement 保留原价与交割状态；
//   - 部分补偿：Compensation 只覆盖已交割数量，FinalSettlement 同时含未交割改价。
func (s *Service) GetCorrectionTrace(ctx context.Context, auctionID, correctionID string) (*CorrectionTrace, error) {
	if auctionID == "" || correctionID == "" {
		return nil, NewError(KindInvalidArgument, "auction id and correction id are required")
	}
	agg, err := s.repo.Load(ctx, auctionID)
	if err != nil {
		return nil, err
	}
	app, ok := agg.Corrections[correctionID]
	if !ok {
		return nil, fmt.Errorf("%w: correction %s", ErrNotFound, correctionID)
	}
	if agg.Clearing == nil {
		return nil, NewError(KindInvalidState, "auction has no clearing result: "+auctionID)
	}
	cur, err := currentSettlement(agg)
	if err != nil {
		return nil, err
	}
	eff := settlementByVersion(agg, app.EffectiveVersion)
	if eff == nil {
		// 尚未批准：EffectiveVersion 为空，挂在基准版本上查看。
		eff = settlementByVersion(agg, app.BaseSettleVersion)
	}

	trace := &CorrectionTrace{
		CorrectionID:     app.CorrectionID,
		BatchID:          app.BatchID,
		Reason:           app.Reason,
		Basis:            app.Basis,
		Handler:          app.Handler,
		PriceVersion:     app.PriceVersion,
		BaseVersion:      app.BaseSettleVersion,
		EffectiveVersion: app.EffectiveVersion,
		CurrentVersion:   cur.Version,
		Status:           app.Status,
		SubmittedAt:      app.SubmittedAt,
		Events:           append([]CorrectionEvent(nil), app.Events...),
		Items:            make([]CorrectionTraceItem, 0, len(app.Items)),
	}
	if app.ApprovedAt != nil {
		t := *app.ApprovedAt
		trace.ApprovedAt = &t
	}

	snapByRef := map[string]CorrectionItemSnapshot{}
	for _, snap := range app.Snapshots {
		snapByRef[snap.ExternalRef] = snap
	}
	compByRef := map[string]*Compensation{}
	for i := range app.Compensations {
		compByRef[app.Compensations[i].ExternalRef] = &app.Compensations[i]
	}

	for _, it := range app.Items {
		item := CorrectionTraceItem{
			ExternalRef:     it.ExternalRef,
			TargetUnitPrice: it.TargetUnitPrice,
			SubmitSnapshot:  snapByRef[it.ExternalRef],
		}
		if f := fillByRef(agg.Clearing, it.ExternalRef); f != nil {
			item.Original = OriginalFillView{
				ExternalRef: f.ExternalRef,
				Bidder:      f.Bidder,
				Seq:         f.Seq,
				Quantity:    f.Quantity,
				UnitPrice:   f.UnitPrice,
				Amount:      f.Amount,
			}
		}
		if e := entryByRef(eff, it.ExternalRef); e != nil {
			final := *e
			item.FinalSettlement = &final
			switch {
			case e.DeliveredQty <= 0:
				item.Scenario = ScenarioUndelivered
			case e.DeliveredQty >= e.MatchedQty:
				item.Scenario = ScenarioDelivered
			default:
				item.Scenario = ScenarioPartial
			}
		}
		if c := compByRef[it.ExternalRef]; c != nil {
			cp := *c
			item.Compensation = &cp
		}
		if cur.Version != eff.Version {
			if e := entryByRef(cur, it.ExternalRef); e != nil {
				cp := *e
				item.CurrentSettlement = &cp
			}
		}
		trace.Items = append(trace.Items, item)
	}
	return trace, nil
}

func settlementByVersion(agg *Aggregate, v SettlementVersion) *Settlement {
	for _, st := range agg.Settlements {
		if st.Version == v {
			return st
		}
	}
	return nil
}

func fillByRef(res *ClearingResult, ref string) *Fill {
	for i := range res.Fills {
		if res.Fills[i].ExternalRef == ref {
			return &res.Fills[i]
		}
	}
	return nil
}
