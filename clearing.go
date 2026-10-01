package auctionclearing

import (
	"sort"
	"time"
)

// clearingSnapshot 是清算时刻的报价快照：只含未撤回、且通过保留价的报价。
// 迟到报价与已撤回报价在进入快照前就被排除，此后的排序与分配不再触碰实时状态。
type clearingSnapshot struct {
	at   time.Time
	bids []*Bid
}

// takeSnapshot 在统一的时间源 now 下，从聚合中取一份不可变快照：
//   - 仅 State == active 的报价（已撤回的不会被“恢复”进来）；
//   - 仅单价 >= 保留价的报价；
//
// 排序：单价降序；同价按首次有效提交时间升序；时间再相同则按外部编号升序，
// 保证任何输入下顺序都是完全确定的（同价同时提交时结果也唯一）。
func takeSnapshot(agg *Aggregate, now time.Time) clearingSnapshot {
	snap := clearingSnapshot{at: now}
	for _, b := range agg.Bids {
		if b.State != BidActive {
			continue
		}
		if b.UnitPrice.Cmp(agg.Auction.ReservePrice) < 0 {
			continue
		}
		snap.bids = append(snap.bids, b)
	}
	sort.SliceStable(snap.bids, func(i, j int) bool {
		a, b := snap.bids[i], snap.bids[j]
		if c := a.UnitPrice.Cmp(b.UnitPrice); c != 0 {
			return c > 0
		}
		if !a.FirstSubmittedAt.Equal(b.FirstSubmittedAt) {
			return a.FirstSubmittedAt.Before(b.FirstSubmittedAt)
		}
		return a.ExternalRef < b.ExternalRef
	})
	return snap
}

// clearFromSnapshot 依据快照按顺序分配可售数量：
// 边界报价（排在可售量恰好耗尽位置的报价）允许部分成交；
// 最终成交量绝不超过可售量。该函数是纯函数，不修改任何报价状态，
// 失败只需不采纳其返回值，天然不存在部分成交残留。
func clearFromSnapshot(a Auction, snap clearingSnapshot) *ClearingResult {
	res := &ClearingResult{
		AuctionID:     a.ID,
		SnapshotAt:    snap.at,
		ClearedAt:     snap.at,
		AvailableQty:  a.AvailableQty,
		ReservePrice:  a.ReservePrice,
		BidCount:      len(snap.bids),
		Fills:         []Fill{},
		TotalProceeds: ZeroMoney(),
	}

	remaining := a.AvailableQty
	seq := 0
	for _, b := range snap.bids {
		if remaining <= 0 {
			break
		}
		fillQty := b.Quantity
		if fillQty > remaining {
			fillQty = remaining // 边界报价部分成交
		}
		if fillQty <= 0 {
			continue
		}
		seq++
		amount := b.UnitPrice.MulQuantity(fillQty)
		res.Fills = append(res.Fills, Fill{
			ExternalRef: b.ExternalRef,
			Bidder:      b.Bidder,
			Quantity:    fillQty,
			UnitPrice:   b.UnitPrice,
			Amount:      amount,
			Seq:         seq,
		})
		res.SoldQty += fillQty
		res.TotalProceeds = res.TotalProceeds.Add(amount)
		remaining -= fillQty
	}
	return res
}
