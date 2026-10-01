package auctionclearing_test

import (
	"fmt"
	"time"

	auctionclearing "github.com/chris64233/go-auction-clearing"
)

// Example 演示一场多单位密封竞价拍卖的完整生命周期：
// 创建 → 报价 → 幂等重报 → 撤回 → 截止清算 → 重复清算读取原结果。
func Example() {
	clock := auctionclearing.NewControlledClock(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	engine, err := auctionclearing.NewEngine(auctionclearing.NewMemoryEventStore(), clock)
	if err != nil {
		panic(err)
	}

	deadline := clock.Now().Add(time.Hour)
	a, err := engine.CreateAuction(10, deadline, auctionclearing.MustParseMoney("5"))
	if err != nil {
		panic(err)
	}
	fmt.Println("auction created:", a.ID, "supply:", a.AvailableQty)

	mk := func(ext, bidder string, qty int64, price string) auctionclearing.BidInput {
		return auctionclearing.BidInput{
			AuctionID: a.ID, ExternalID: ext, Bidder: bidder,
			Qty: qty, Price: auctionclearing.MustParseMoney(price),
		}
	}
	r1, _ := engine.SubmitBid(mk("b1", "Alice", 4, "10"))
	r2, _ := engine.SubmitBid(mk("b2", "Bob", 8, "9"))  // 边界：4+8>10，部分成交 6
	r3, _ := engine.SubmitBid(mk("b3", "Carl", 2, "4")) // 低于保留价
	fmt.Println("outcomes:", r1.Outcome, r2.Outcome, r3.Outcome)

	// 相同内容重复提交：幂等返回原结果。
	replay, _ := engine.SubmitBid(mk("b1", "Alice", 4, "10"))
	fmt.Println("replay:", replay.Outcome)

	// 撤回 b3。
	wd, _ := engine.WithdrawBid(a.ID, "b3")
	fmt.Println("withdraw:", wd.Outcome)

	// 到达截止点并清算。
	clock.Set(deadline)
	st, err := engine.SettleAuction(a.ID)
	if err != nil {
		panic(err)
	}
	fmt.Println("settle:", st.Outcome, "sold:", st.Data.SoldQty,
		"price:", st.Data.ClearingPrice, "proceeds:", st.Data.TotalProceeds)
	for _, f := range st.Data.Fills {
		fmt.Printf("fill: %s qty=%d rank=%d\n", f.ExternalID, f.FilledQty, f.Rank)
	}

	// 重复清算：读取同一份结果。
	again, _ := engine.SettleAuction(a.ID)
	fmt.Println("settle again:", again.Outcome)

	// Output:
	// auction created: 1 supply: 10
	// outcomes: accepted accepted accepted
	// replay: replayed
	// withdraw: withdrawn
	// settle: settled sold: 10 price: 9 proceeds: 90
	// fill: b1 qty=4 rank=1
	// fill: b2 qty=6 rank=2
	// settle again: replayed
}
