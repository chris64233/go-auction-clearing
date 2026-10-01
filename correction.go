package auctionclearing

import (
	"context"
	"fmt"
)

// CompensationGateway 是已交割成交差额补偿所走的外部结算通道。
// 它可能失败；批准更正时若补偿结算失败，补偿单与更正都停在待处理，
// 原成交不受影响，可稍后用 RetryCompensation 重试。
type CompensationGateway interface {
	SettleCompensation(ctx context.Context, auctionID, externalRef string, amount Money) error
}

// noopCompensationGateway 默认通道：补偿结算总是成功。
type noopCompensationGateway struct{}

func (noopCompensationGateway) SettleCompensation(context.Context, string, string, Money) error {
	return nil
}

// ServiceOption 配置 Service 的可选依赖。
type ServiceOption func(*Service)

// WithCompensationGateway 注入补偿结算通道（测试可注入失败通道）。
func WithCompensationGateway(g CompensationGateway) ServiceOption {
	return func(svc *Service) {
		if g != nil {
			svc.compGateway = g
		}
	}
}

// SettleOutcome 生成结算的结果；Replayed=true 表示结算此前已生成。
type SettleOutcome struct {
	Settlement *Settlement `json:"settlement"`
	Replayed   bool        `json:"replayed"`
}

// SubmitCorrectionParams 更正申请入参。
type SubmitCorrectionParams struct {
	// CorrectionID 外部更正号，批次内唯一的幂等键。
	CorrectionID string `json:"correction_id"`
	Reason       string `json:"reason"`
	Basis        string `json:"basis"`   // 更正依据（即便价格不变也必填并留痕）
	Handler      string `json:"handler"` // 处理人
	Items        []CorrectionItem
}

// SubmitCorrectionOutcome 更正申请结果；Replayed=true 为同号幂等重放。
type SubmitCorrectionOutcome struct {
	Application *CorrectionApplication `json:"application"`
	Replayed    bool                   `json:"replayed"`
}

// ApproveCorrectionParams 批准更正入参；Handler/Basis 用于审计留痕。
type ApproveCorrectionParams struct {
	Handler string `json:"handler"`
	Basis   string `json:"basis"`
}

// SettleAuction 在拍卖已清算后生成首版结算（成交确认/冻结/交割的前提）。
// 结算与清算是两件事：清算给出不可变的撮合结果，结算承载成交后的处理流水。
// 重复调用幂等，返回同一份首版结算。
func (s *Service) SettleAuction(ctx context.Context, auctionID string) (*SettleOutcome, error) {
	if auctionID == "" {
		return nil, NewError(KindInvalidArgument, "auction id is required")
	}
	out := &SettleOutcome{}
	_, err := s.repo.Update(ctx, auctionID, func(agg *Aggregate) error {
		if agg.Clearing == nil {
			return NewError(KindInvalidState, "auction has not been cleared yet: "+auctionID)
		}
		if len(agg.Settlements) > 0 {
			out.Settlement = agg.Settlements[0]
			out.Replayed = true
			return nil
		}
		st := newSettlementFromResult(agg.Clearing, s.clock.Now())
		agg.Settlements = append(agg.Settlements, st)
		out.Settlement = st
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// currentSettlement 返回当前有效结算版本；尚未结算返回 ErrNotSettled。
func currentSettlement(agg *Aggregate) (*Settlement, error) {
	for i := len(agg.Settlements) - 1; i >= 0; i-- {
		if !agg.Settlements[i].Superseded {
			return agg.Settlements[i], nil
		}
	}
	return nil, ErrNotSettled
}

func entryByRef(st *Settlement, ref string) *SettlementEntry {
	for i := range st.Entries {
		if st.Entries[i].ExternalRef == ref {
			return &st.Entries[i]
		}
	}
	return nil
}

// ConfirmFill 完成一笔成交的成交确认。幂等：已确认返回现状。
func (s *Service) ConfirmFill(ctx context.Context, auctionID, externalRef string) (*SettlementEntry, error) {
	return s.mutateEntry(ctx, auctionID, externalRef, func(e *SettlementEntry) error {
		e.Confirmed = true
		return nil
	})
}

// FreezeFunds 对一笔已确认成交冻结资金。未确认先冻结属于状态错误。
// 幂等：已冻结返回现状。
func (s *Service) FreezeFunds(ctx context.Context, auctionID, externalRef string) (*SettlementEntry, error) {
	return s.mutateEntry(ctx, auctionID, externalRef, func(e *SettlementEntry) error {
		if !e.Confirmed {
			return NewError(KindInvalidState, "fill must be confirmed before funds freeze: "+externalRef)
		}
		e.FundsFrozen = true
		return nil
	})
}

// MarkDelivered 记录一笔成交的交割数量（增量，不得超过未交割余量）。
// 交割按“交割时刻有效结算版本的单价”锁定金额；已交割事实随后续更正
// 原样继承，绝不回滚。
func (s *Service) MarkDelivered(ctx context.Context, auctionID, externalRef string, qty int64) (*SettlementEntry, error) {
	if qty < 0 {
		return nil, NewError(KindInvalidArgument, "delivered quantity must not be negative")
	}
	return s.mutateEntry(ctx, auctionID, externalRef, func(e *SettlementEntry) error {
		if qty > e.UndeliveredQty() {
			return NewError(KindInvalidState, fmt.Sprintf(
				"delivery qty %d exceeds undelivered qty %d for fill %s", qty, e.UndeliveredQty(), externalRef))
		}
		if qty > 0 {
			e.DeliveredQty += qty
			e.DeliveredAmount = e.DeliveredAmount.Add(e.EffectiveUnitPrice.MulQuantity(qty))
			if e.DeliveredQty >= e.MatchedQty {
				now := s.clock.Now()
				e.DeliveredAt = &now
			}
		}
		return nil
	})
}

// mutateEntry 在当前有效结算版本上对单笔成交做一步状态推进。
// 与成交确认、资金冻结、更正批准共用同一把批次锁，保证同一订单的多个
// 动作同时到达时串行化，任一时刻只有一套有效成交结果。
func (s *Service) mutateEntry(ctx context.Context, auctionID, externalRef string, apply func(e *SettlementEntry) error) (*SettlementEntry, error) {
	if auctionID == "" || externalRef == "" {
		return nil, NewError(KindInvalidArgument, "auction id and external ref are required")
	}
	var out *SettlementEntry
	_, err := s.repo.Update(ctx, auctionID, func(agg *Aggregate) error {
		st, err := currentSettlement(agg)
		if err != nil {
			return err
		}
		e := entryByRef(st, externalRef)
		if e == nil {
			return fmt.Errorf("%w: fill %s", ErrNotFound, externalRef)
		}
		if err := apply(e); err != nil {
			return err
		}
		out = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetSettlement 查询当前有效结算版本（含每笔成交的确认/冻结/交割状态）。
func (s *Service) GetSettlement(ctx context.Context, auctionID string) (*Settlement, error) {
	if auctionID == "" {
		return nil, NewError(KindInvalidArgument, "auction id is required")
	}
	agg, err := s.repo.Load(ctx, auctionID)
	if err != nil {
		return nil, err
	}
	return currentSettlement(agg)
}
