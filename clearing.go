package auctionclearing

import (
	"sort"
	"time"
)

// clearingCandidate 是参与排序分配的有效报价视图。
type clearingCandidate struct {
	externalID  string
	bidder      string
	qty         int64
	price       Money
	submittedAt time.Time
}

// settleFromSnapshot 对同一份报价快照执行纯函数式清算。
//
// 规则：
//  1. 只取 Status == active 的报价；已撤回的报价不进入分配（也不能被恢复）。
//  2. 只保留单价 >= 最低成交价（reserve）的报价。
//  3. 排序键：单价降序；同价按“首次有效提交时间”升序；时间再相同则按外部编号兜底，
//     保证任意输入下顺序唯一确定。
//  4. 按排序依次分配，成交数量累计不得超过可售量；恰好位于边界的报价允许部分成交。
//  5. 所有成交按统一价格（边界报价单价）计；无成交时清算价为 0。
//
// 该函数不做任何 I/O，也不会修改输入切片以外的状态；调用方负责保证快照原子性，
// 因此“执行失败遗留部分成交”在结构上不可能发生。
func settleFromSnapshot(availableQty int64, reserve Money, bids []Bid, settledAt time.Time) (Settlement, error) {
	candidates := make([]clearingCandidate, 0, len(bids))
	for _, b := range bids {
		if b.Status != BidActive {
			continue
		}
		if b.Price.Cmp(reserve) < 0 {
			continue
		}
		candidates = append(candidates, clearingCandidate{
			externalID:  b.ExternalID,
			bidder:      b.Bidder,
			qty:         b.Qty,
			price:       b.Price,
			submittedAt: b.SubmittedAt,
		})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		pi, pj := candidates[i].price, candidates[j].price
		if pi != pj {
			return pi > pj // 单价降序
		}
		ti, tj := candidates[i].submittedAt, candidates[j].submittedAt
		if !ti.Equal(tj) {
			return ti.Before(tj) // 同价：首次有效提交在先
		}
		return candidates[i].externalID < candidates[j].externalID // 确定性兜底
	})

	remaining := availableQty
	fills := make([]Fill, 0)
	var total Money
	rank := 0

	for _, c := range candidates {
		if remaining <= 0 {
			break
		}
		fillQty := c.qty
		if fillQty > remaining {
			fillQty = remaining // 边界报价部分成交
		}
		amount, err := c.price.Mul(fillQty)
		if err != nil {
			return Settlement{}, err
		}
		t, err := total.Add(amount)
		if err != nil {
			return Settlement{}, err
		}
		total = t
		remaining -= fillQty
		rank++
		fills = append(fills, Fill{
			ExternalID:   c.externalID,
			Bidder:       c.bidder,
			Price:        c.price,
			RequestedQty: c.qty,
			FilledQty:    fillQty,
			Rank:         rank,
		})
	}

	clearingPrice := Money(0)
	if len(fills) > 0 {
		// 统一价格清算：边界报价（最后一份成交）的单价即清算价。
		clearingPrice = fills[len(fills)-1].Price
		for i := range fills {
			fills[i].Price = clearingPrice
		}
		// 用统一清算价重算总额。
		total, _ = clearingPrice.Mul(availableQty - remaining)
	}

	return Settlement{
		ClearingPrice: clearingPrice,
		SoldQty:       availableQty - remaining,
		TotalProceeds: total,
		Fills:         fills,
		SettledAt:     settledAt,
	}, nil
}
