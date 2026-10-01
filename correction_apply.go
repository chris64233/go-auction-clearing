package auctionclearing

import (
	"context"
)

// SubmitCorrection 登记一张成交更正申请。
//
// 申请保存成交清单、价格版本与提交时的结算状态；更正范围受竞价批次（拍卖）
// 与结算版本限制：
//   - 相同更正号、相同批次/版本/内容重复提交：幂等返回原结果（Replayed=true）；
//   - 相同更正号但价格版本、基准结算版本或申请内容不一致：KindConflict；
//   - 已全额交割的成交申请阶段允许登记，批准时自动转为补偿单，
//     不会尝试改写其原结算。
func (s *Service) SubmitCorrection(ctx context.Context, auctionID string, p SubmitCorrectionParams) (*SubmitCorrectionOutcome, error) {
	if auctionID == "" {
		return nil, NewError(KindInvalidArgument, "auction id is required")
	}
	if p.CorrectionID == "" {
		return nil, NewError(KindInvalidArgument, "correction id is required")
	}
	if p.Reason == "" {
		return nil, NewError(KindInvalidArgument, "correction reason is required")
	}
	if p.Basis == "" {
		return nil, NewError(KindInvalidArgument, "correction basis is required even if price is unchanged")
	}
	if p.Handler == "" {
		return nil, NewError(KindInvalidArgument, "correction handler is required")
	}
	if len(p.Items) == 0 {
		return nil, NewError(KindInvalidArgument, "correction items are required")
	}
	seen := map[string]bool{}
	for _, it := range p.Items {
		if it.ExternalRef == "" {
			return nil, NewError(KindInvalidArgument, "correction item external ref is required")
		}
		if seen[it.ExternalRef] {
			return nil, NewError(KindInvalidArgument, "duplicate fill in correction items: "+it.ExternalRef)
		}
		seen[it.ExternalRef] = true
		if it.TargetUnitPrice.Cmp(ZeroMoney()) < 0 {
			return nil, NewError(KindInvalidArgument, "target unit price must not be negative")
		}
	}

	out := &SubmitCorrectionOutcome{}
	_, err := s.repo.Update(ctx, auctionID, func(agg *Aggregate) error {
		if agg.Corrections == nil {
			agg.Corrections = map[string]*CorrectionApplication{}
		}
		st, err := currentSettlement(agg)
		if err != nil {
			return err
		}
		if existing, ok := agg.Corrections[p.CorrectionID]; ok {
			// 幂等判定先于一切：同号必须仍挂在同一批次、同一价格版本与
			// 同一基准结算版本上；批次（拍卖）或版本变化一律报冲突，
			// 同版本下再比较申请内容是否完全一致。
			if existing.BaseSettleVersion != st.Version || existing.PriceVersion != st.PriceVersion {
				return ErrCorrectionConflict
			}
			if !correctionRequestEqual(existing, p) {
				return ErrCorrectionConflict
			}
			out.Application = existing
			out.Replayed = true
			return nil
		}
		app := &CorrectionApplication{
			CorrectionID:      p.CorrectionID,
			AuctionID:         auctionID,
			Reason:            p.Reason,
			Basis:             p.Basis,
			Handler:           p.Handler,
			BatchID:           auctionID,
			PriceVersion:      st.PriceVersion,
			BaseSettleVersion: st.Version,
			Items:             append([]CorrectionItem(nil), p.Items...),
			Status:            CorrectionSubmitted,
			SubmittedAt:       s.clock.Now(),
			Snapshots:         make([]CorrectionItemSnapshot, 0, len(p.Items)),
		}
		for _, it := range p.Items {
			e := entryByRef(st, it.ExternalRef)
			if e == nil {
				return NewError(KindNotFound, "fill not found in current settlement: "+it.ExternalRef)
			}
			app.Snapshots = append(app.Snapshots, CorrectionItemSnapshot{
				ExternalRef:     e.ExternalRef,
				Seq:             e.Seq,
				MatchedQty:      e.MatchedQty,
				DeliveredQty:    e.DeliveredQty,
				OriginalPrice:   e.OriginalUnitPrice,
				PriceVersion:    e.PriceVersion,
				SettlementState: e.Status(),
			})
		}
		agg.Corrections[p.CorrectionID] = app
		out.Application = app
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// correctionRequestEqual 判定同号更正是否为同一请求的重试：
// 原因/依据/处理人以及逐笔成交号与目标单价必须一致。
func correctionRequestEqual(a *CorrectionApplication, p SubmitCorrectionParams) bool {
	if a.Reason != p.Reason || a.Basis != p.Basis || a.Handler != p.Handler {
		return false
	}
	if len(a.Items) != len(p.Items) {
		return false
	}
	existing := map[string]Money{}
	for _, it := range a.Items {
		existing[it.ExternalRef] = it.TargetUnitPrice
	}
	for _, it := range p.Items {
		price, ok := existing[it.ExternalRef]
		if !ok || price.Cmp(it.TargetUnitPrice) != 0 {
			return false
		}
	}
	return true
}
