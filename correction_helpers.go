package auctionclearing

import (
	"context"
	"fmt"
	"time"
)

// applyCorrectionChange 在批准事务内执行一次有价格变化的更正。
//
// 它先对已交割部分逐笔调用补偿通道：
//   - 全部成功：旧结算版本置为 superseded、追加新版本、更正置为 completed；
//   - 任一失败：**不切换结算版本**（原结算继续可用，未交割部分也不会提前
//     生效），只把更正与补偿单留在 pending 并记录失败原因，等待
//     RetryCompensation 从补偿步骤重新执行整个原子动作。
func (s *Service) applyCorrectionChange(
	ctx context.Context,
	agg *Aggregate,
	app *CorrectionApplication,
	cur *Settlement,
	targets map[string]Money,
	handler, basis string,
	now time.Time,
) error {
	next := buildNextSettlement(cur, targets, now)
	comps := buildCompensations(app.Items, next, now)

	allSettled := true
	for i := range comps {
		c := &comps[i]
		if err := s.compGateway.SettleCompensation(ctx, agg.Auction.ID, c.ExternalRef, c.Amount); err != nil {
			c.Status = CompensationPending
			c.FailureNote = err.Error()
			allSettled = false
			continue
		}
		c.Status = CompensationSettled
		settledAt := now
		c.SettledAt = &settledAt
	}

	// 批准时间在进入本函数前已记录；补偿失败也要留下“批准已尝试”的审计。
	if !allSettled {
		app.Status = CorrectionPending
		app.Compensations = append(app.Compensations, comps...)
		app.Events = append(app.Events, CorrectionEvent{
			At:          now,
			Handler:     handler,
			Basis:       basis,
			Changed:     true,
			FromVersion: cur.Version,
			ToVersion:   cur.Version,
			Note:        compensationNote(comps, false),
		})
		return nil
	}

	cur.Superseded = true
	agg.Settlements = append(agg.Settlements, next)
	app.Compensations = append(app.Compensations, comps...)
	app.EffectiveVersion = next.Version
	app.Status = CorrectionCompleted
	app.Events = append(app.Events, CorrectionEvent{
		At:          now,
		Handler:     handler,
		Basis:       basis,
		Changed:     true,
		FromVersion: cur.Version,
		ToVersion:   next.Version,
		Note:        compensationNote(comps, true),
	})
	return nil
}

// buildNextSettlement 构造新结算版本：继承全部原始撮合信息与交割状态，
// 仅覆盖允许更正的结算字段（结算单价、结算金额）。
func buildNextSettlement(cur *Settlement, targets map[string]Money, now time.Time) *Settlement {
	next := &Settlement{
		AuctionID:    cur.AuctionID,
		PriceVersion: cur.PriceVersion, // 更正不重新撮合定价，价格版本不变
		Version:      cur.Version + 1,
		CreatedAt:    now,
		Entries:      make([]SettlementEntry, len(cur.Entries)),
		TotalSettled: ZeroMoney(),
	}
	copy(next.Entries, cur.Entries)
	for i := range next.Entries {
		e := &next.Entries[i]
		if target, hit := targets[e.ExternalRef]; hit && e.UndeliveredQty() > 0 {
			// 只有未交割部分允许直接改结算单价；已全额交割的成交
			// 只能另做补偿，其结算单价必须保持原价。
			e.EffectiveUnitPrice = target
		}
		// 结算金额 = 已交割锁定金额 + 未交割部分按新单价结算；
		// 已交割金额原样继承，已交割部分由补偿单单独补差。
		e.SettledAmount = e.DeliveredAmount.Add(e.EffectiveUnitPrice.MulQuantity(e.UndeliveredQty()))
		next.TotalSettled = next.TotalSettled.Add(e.SettledAmount)
	}
	return next
}

// buildCompensations 为每笔涉及更正且已有交割的成交生成补偿单：
// 差额 = 交割锁定金额 - 目标单价*已交割数量（有符号，精确计算）。
// 完全未交割的成交直接在新版本改价，不产生补偿；差额为 0 同样不产生。
func buildCompensations(items []CorrectionItem, next *Settlement, now time.Time) []Compensation {
	var comps []Compensation
	for _, it := range items {
		e := entryByRef(next, it.ExternalRef)
		if e == nil || e.DeliveredQty <= 0 {
			continue
		}
		desired := it.TargetUnitPrice.MulQuantity(e.DeliveredQty)
		delta := e.DeliveredAmount.Sub(desired)
		if delta.IsZero() {
			continue
		}
		comps = append(comps, Compensation{
			ExternalRef:  e.ExternalRef,
			Seq:          e.Seq,
			Quantity:     e.DeliveredQty,
			LockedAmount: e.DeliveredAmount,
			TargetPrice:  it.TargetUnitPrice,
			Amount:       delta,
			Status:       CompensationPending,
			CreatedAt:    now,
		})
	}
	return comps
}

func compensationNote(comps []Compensation, allSettled bool) string {
	switch {
	case len(comps) == 0:
		return "no delivered quantity; price corrected directly on new version"
	case allSettled:
		return fmt.Sprintf("%d compensation(s) settled", len(comps))
	default:
		pending := 0
		for _, c := range comps {
			if c.Status == CompensationPending {
				pending++
			}
		}
		return fmt.Sprintf("%d of %d compensation(s) pending settlement", pending, len(comps))
	}
}

// RetryCompensation 重试一张待处理更正下尚未结清的补偿单。
// 全部结清后更正转为 completed；仍有失败则保持 pending 并返回现状。
func (s *Service) RetryCompensation(ctx context.Context, auctionID, correctionID string) (*CorrectionApplication, error) {
	if auctionID == "" || correctionID == "" {
		return nil, NewError(KindInvalidArgument, "auction id and correction id are required")
	}
	_, err := s.repo.Update(ctx, auctionID, func(agg *Aggregate) error {
		app, ok := agg.Corrections[correctionID]
		if !ok {
			return fmt.Errorf("%w: correction %s", ErrNotFound, correctionID)
		}
		if app.Status != CorrectionPending {
			return NewError(KindInvalidState, "correction is not awaiting compensation: "+correctionID)
		}
		cur, err := currentSettlement(agg)
		if err != nil {
			return err
		}
		// pending 意味着补偿失败时未切换版本；基准版本理应仍是当前版本。
		if app.BaseSettleVersion != cur.Version {
			return ErrStaleSettlement
		}
		// 重试从补偿步骤重新执行整个原子动作：清空上一次失败的 pending 补偿单
		// 与事件，按当前时间重新结算，成功后才切换版本。
		app.Compensations = app.Compensations[:0]
		app.Events = app.Events[:len(app.Events)]
		// 去掉最后一条“pending”审计事件（Changed=true 但未切换版本）。
		if n := len(app.Events); n > 0 && app.Events[n-1].FromVersion == app.Events[n-1].ToVersion {
			app.Events = app.Events[:n-1]
		}
		targets := make(map[string]Money, len(app.Items))
		for _, it := range app.Items {
			targets[it.ExternalRef] = it.TargetUnitPrice
		}
		return s.applyCorrectionChange(ctx, agg, app, cur, targets, app.Handler, app.Basis, s.clock.Now())
	})
	if err != nil {
		return nil, err
	}
	return s.GetCorrection(ctx, auctionID, correctionID)
}

// GetCorrection 查询一张更正申请（含快照、补偿与审计事件）。
func (s *Service) GetCorrection(ctx context.Context, auctionID, correctionID string) (*CorrectionApplication, error) {
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
	return app, nil
}

// ListCorrections 列出一个竞价批次下的全部更正申请（按提交时间先后）。
func (s *Service) ListCorrections(ctx context.Context, auctionID string) ([]*CorrectionApplication, error) {
	if auctionID == "" {
		return nil, NewError(KindInvalidArgument, "auction id is required")
	}
	agg, err := s.repo.Load(ctx, auctionID)
	if err != nil {
		return nil, err
	}
	out := make([]*CorrectionApplication, 0, len(agg.Corrections))
	for _, c := range agg.Corrections {
		out = append(out, c)
	}
	// 按提交时间、再按更正号排序，保证输出确定。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if a.SubmittedAt.After(b.SubmittedAt) ||
				(a.SubmittedAt.Equal(b.SubmittedAt) && a.CorrectionID > b.CorrectionID) {
				out[j-1], out[j] = b, a
			} else {
				break
			}
		}
	}
	return out, nil
}
