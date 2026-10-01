package auctionclearing

import (
	"context"
	"fmt"
)

// ApproveCorrection 批准并执行一张更正申请，返回申请与是否为幂等重放。
//
// 批准与成交确认、资金冻结共用同一批次事务，保证一个订单只有一套有效
// 成交结果：
//   - 未交割部分：只改结算字段（结算单价/金额），生成新结算版本，
//     原始撮合信息与已交割状态按原版本继承；
//   - 已交割部分：原成交不动，生成补偿单，差额走补偿通道；
//   - 价格未变：不改结算，但仍记录更正依据与处理人（审计事件），
//     更正置为 completed，绝不只返回“无变化”；
//   - 补偿结算失败：补偿单保持 pending、更正停在 pending，原成交仍可查询；
//   - 申请挂接的结算版本已被取代：KindConflict（ErrStaleSettlement）；
//   - 已批准的申请重复批准：幂等返回原结果（Replayed=true）。
func (s *Service) ApproveCorrection(ctx context.Context, auctionID, correctionID string, p ApproveCorrectionParams) (*CorrectionApplication, bool, error) {
	if auctionID == "" || correctionID == "" {
		return nil, false, NewError(KindInvalidArgument, "auction id and correction id are required")
	}
	var replayed bool
	_, err := s.repo.Update(ctx, auctionID, func(agg *Aggregate) error {
		app, ok := agg.Corrections[correctionID]
		if !ok {
			return fmt.Errorf("%w: correction %s", ErrNotFound, correctionID)
		}
		if app.Status != CorrectionSubmitted {
			// 已批准（completed/pending）重复批准：原样返回，不重复执行。
			replayed = true
			return nil
		}
		cur, err := currentSettlement(agg)
		if err != nil {
			return err
		}
		// 更正范围受批次与结算版本限制：基准版本必须仍是当前有效版本。
		if app.BaseSettleVersion != cur.Version || app.PriceVersion != cur.PriceVersion {
			return ErrStaleSettlement
		}

		handler := p.Handler
		if handler == "" {
			handler = app.Handler
		}
		basis := p.Basis
		if basis == "" {
			basis = app.Basis
		}
		now := s.clock.Now()

		// 仅当某笔目标单价不同于当前有效结算单价时才算改变。
		changed := false
		targets := make(map[string]Money, len(app.Items))
		for _, it := range app.Items {
			e := entryByRef(cur, it.ExternalRef)
			if e == nil {
				return NewError(KindNotFound, "fill vanished from settlement: "+it.ExternalRef)
			}
			targets[it.ExternalRef] = it.TargetUnitPrice
			if it.TargetUnitPrice.Cmp(e.EffectiveUnitPrice) != 0 {
				changed = true
			}
		}

		approvedAt := now
		app.ApprovedAt = &approvedAt

		if !changed {
			// 无价格变化：不产生新版本，原结算继续可用；
			// 但依据与处理人必须作为审计事件留痕，不能只回“无变化”。
			app.Status = CorrectionCompleted
			app.EffectiveVersion = cur.Version
			app.Events = append(app.Events, CorrectionEvent{
				At:          now,
				Handler:     handler,
				Basis:       basis,
				Changed:     false,
				FromVersion: cur.Version,
				ToVersion:   cur.Version,
				Note:        "target price identical to effective price; no settlement change",
			})
			return nil
		}

		if err := s.applyCorrectionChange(ctx, agg, app, cur, targets, handler, basis, now); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	app, err := s.GetCorrection(ctx, auctionID, correctionID)
	if err != nil {
		return nil, false, err
	}
	return app, replayed, nil
}
