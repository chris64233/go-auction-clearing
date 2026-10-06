package auctionclearing

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func released(amount int64) MarginResult {
	return MarginResult{Action: MarginReleased, AmountCents: amount}
}

func forfeited(amount int64) MarginResult {
	return MarginResult{Action: MarginForfeited, AmountCents: amount}
}

// setupCleared 创建一场已清算的拍卖，并按给定数量登记成交。
func setupCleared(t *testing.T, svc *Service, auctionID string, quantities ...int64) []*Trade {
	t.Helper()
	if _, err := svc.CreateAuction(auctionID); err != nil {
		t.Fatalf("CreateAuction: %v", err)
	}
	var trades []*Trade
	for i, q := range quantities {
		tr, err := svc.RecordTrade(auctionID, "buyer", "seller", q, 1000+int64(i))
		if err != nil {
			t.Fatalf("RecordTrade: %v", err)
		}
		trades = append(trades, tr)
	}
	if _, err := svc.ClearAuction(auctionID); err != nil {
		t.Fatalf("ClearAuction: %v", err)
	}
	return trades
}

func mustBatch(t *testing.T, svc *Service, auctionID string, trades ...*Trade) *DeliveryBatch {
	t.Helper()
	ids := make([]string, 0, len(trades))
	for _, tr := range trades {
		ids = append(ids, tr.ID)
	}
	b, err := svc.CreateBatch(auctionID, ids, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	return b
}

func TestCreateBatchRules(t *testing.T) {
	svc := NewService(NewStore(""))
	trades := setupCleared(t, svc, "A1", 10, 20)

	// 冻结数量、价格与结算版本。
	b := mustBatch(t, svc, "A1", trades[0])
	if b.Version != 0 || b.Status != BatchOpen {
		t.Fatalf("unexpected batch: %+v", b)
	}
	if got := b.Items[0].PlannedQuantity; got != 10 {
		t.Fatalf("planned quantity = %d, want 10", got)
	}
	if got := b.Items[0].PriceCents; got != 1000 {
		t.Fatalf("frozen price = %d, want 1000", got)
	}

	// 一笔成交只能属于一个未完成批次。
	if _, err := svc.CreateBatch("A1", []string{trades[0].ID}, time.Now().Add(time.Hour)); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("duplicate open batch err = %v, want ErrInvalidState", err)
	}

	// 未清算的拍卖不能建批。
	if _, err := svc.CreateAuction("A2"); err != nil {
		t.Fatal(err)
	}
	tr, err := svc.RecordTrade("A2", "b", "s", 5, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateBatch("A2", []string{tr.ID}, time.Now().Add(time.Hour)); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("uncleared auction err = %v, want ErrInvalidState", err)
	}

	// 已全部交割的成交不能再次加入新批次。
	if _, _, err := svc.ConfirmDelivery("op-full", b.ID, trades[0].ID, 10, released(500)); err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}
	if _, err := svc.CloseBatch(b.ID); err != nil {
		t.Fatalf("CloseBatch: %v", err)
	}
	if _, err := svc.CreateBatch("A1", []string{trades[0].ID}, time.Now().Add(time.Hour)); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("fully delivered trade err = %v, want ErrInvalidState", err)
	}
}

func TestPartialDelivery(t *testing.T) {
	svc := NewService(NewStore(""))
	trades := setupCleared(t, svc, "A1", 10)
	b := mustBatch(t, svc, "A1", trades[0])

	// 部分交割只减少剩余数量，不能把成交伪装成完成。
	if _, _, err := svc.ConfirmDelivery("op-1", b.ID, trades[0].ID, 4, released(200)); err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}
	view, err := svc.GetBatchView(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	item := view.Items[0]
	if item.Status != ItemPartial {
		t.Fatalf("status = %s, want PARTIAL", item.Status)
	}
	if item.DeliveredQuantity != 4 || item.RemainingQuantity != 6 {
		t.Fatalf("delivered/remaining = %d/%d, want 4/6", item.DeliveredQuantity, item.RemainingQuantity)
	}

	// 部分交割后批次不能关闭。
	if _, err := svc.CloseBatch(b.ID); !errors.Is(err, ErrBatchNotClosable) {
		t.Fatalf("CloseBatch err = %v, want ErrBatchNotClosable", err)
	}

	// 超出剩余数量的确认被拒绝。
	if _, _, err := svc.ConfirmDelivery("op-2", b.ID, trades[0].ID, 7, released(300)); !errors.Is(err, ErrValidation) {
		t.Fatalf("over-delivery err = %v, want ErrValidation", err)
	}

	// 补足剩余数量后成交完成，批次可以关闭。
	if _, _, err := svc.ConfirmDelivery("op-3", b.ID, trades[0].ID, 6, released(300)); err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}
	view, _ = svc.GetBatchView(b.ID)
	if view.Items[0].Status != ItemCompleted || view.Items[0].RemainingQuantity != 0 {
		t.Fatalf("item = %+v, want COMPLETED with 0 remaining", view.Items[0])
	}
	if got := svc.store.Trades[trades[0].ID].DeliveredQuantity; got != 10 {
		t.Fatalf("trade delivered = %d, want 10", got)
	}
	if len(view.Items[0].MarginRecords) != 2 {
		t.Fatalf("margin records = %d, want 2", len(view.Items[0].MarginRecords))
	}
	closed, err := svc.CloseBatch(b.ID)
	if err != nil {
		t.Fatalf("CloseBatch: %v", err)
	}
	if closed.Status != BatchClosed || closed.ClosedAt == nil {
		t.Fatalf("batch = %+v, want CLOSED with ClosedAt", closed)
	}
}

func TestCorrectionInvalidatesBatch(t *testing.T) {
	svc := NewService(NewStore(""))
	trades := setupCleared(t, svc, "A1", 10, 20)
	b := mustBatch(t, svc, "A1", trades...)

	// 更正先生效：结算版本递增，旧批次不得继续确认。
	c, err := svc.CorrectTrade(trades[0].ID, 8, 1200, "price fix")
	if err != nil {
		t.Fatalf("CorrectTrade: %v", err)
	}
	if c.Version != 1 {
		t.Fatalf("correction version = %d, want 1", c.Version)
	}
	if _, _, err := svc.ConfirmDelivery("op-1", b.ID, trades[0].ID, 5, released(100)); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("confirm on stale batch err = %v, want ErrVersionConflict", err)
	}

	// 过期的确认不能产生孤立的保证金记录。
	if got := len(svc.store.MarginRecords); got != 0 {
		t.Fatalf("margin records = %d, want 0", got)
	}

	// 视图标记批次已过期，并关联到更正记录。
	view, err := svc.GetBatchView(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Stale || view.CurrentVersion != 1 {
		t.Fatalf("view stale=%v current=%d, want stale at version 1", view.Stale, view.CurrentVersion)
	}
	if len(view.Corrections) != 1 || view.Corrections[0].TradeID != trades[0].ID {
		t.Fatalf("corrections = %+v, want 1 for trade %s", view.Corrections, trades[0].ID)
	}

	// 明确取消是过期批次的清理路径；取消后批次可关闭。
	if _, _, err := svc.CancelDelivery("op-c1", b.ID, trades[0].ID, "batch stale", forfeited(50)); err != nil {
		t.Fatalf("CancelDelivery: %v", err)
	}
	if _, _, err := svc.CancelDelivery("op-c2", b.ID, trades[1].ID, "batch stale", forfeited(60)); err != nil {
		t.Fatalf("CancelDelivery: %v", err)
	}
	if _, err := svc.CloseBatch(b.ID); err != nil {
		t.Fatalf("CloseBatch: %v", err)
	}

	// 已取消的成交剩余数量仍可在新批次中交割（未交割部分不丢失）。
	nb, err := svc.CreateBatch("A1", []string{trades[0].ID}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateBatch after cancel: %v", err)
	}
	if got := nb.Items[0].PlannedQuantity; got != 8 {
		t.Fatalf("new batch planned = %d, want corrected 8", got)
	}
}

func TestLateConfirmationAfterClose(t *testing.T) {
	svc := NewService(NewStore(""))
	trades := setupCleared(t, svc, "A1", 10)
	b := mustBatch(t, svc, "A1", trades[0])

	if _, _, err := svc.ConfirmDelivery("op-1", b.ID, trades[0].ID, 10, released(500)); err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}
	if _, err := svc.CloseBatch(b.ID); err != nil {
		t.Fatalf("CloseBatch: %v", err)
	}

	// 批次先关闭：迟到确认返回明确冲突。
	if _, _, err := svc.ConfirmDelivery("op-late", b.ID, trades[0].ID, 1, released(50)); !errors.Is(err, ErrBatchClosed) {
		t.Fatalf("late confirm err = %v, want ErrBatchClosed", err)
	}
	if _, _, err := svc.CancelDelivery("op-late-c", b.ID, trades[0].ID, "late", forfeited(10)); !errors.Is(err, ErrBatchClosed) {
		t.Fatalf("late cancel err = %v, want ErrBatchClosed", err)
	}

	// 迟到操作不能产生孤立的保证金记录。
	if got := len(svc.store.MarginRecords); got != 1 {
		t.Fatalf("margin records = %d, want 1", got)
	}
}

func TestIdempotentReplay(t *testing.T) {
	svc := NewService(NewStore(""))
	trades := setupCleared(t, svc, "A1", 10, 20)
	b := mustBatch(t, svc, "A1", trades...)

	conf, rec, err := svc.ConfirmDelivery("op-1", b.ID, trades[0].ID, 4, released(200))
	if err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}

	// 同号同内容重放返回原结果，不重复扣减、不重复生成记录。
	conf2, rec2, err := svc.ConfirmDelivery("op-1", b.ID, trades[0].ID, 4, released(200))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if conf2 != conf || rec2 != rec {
		t.Fatalf("replay returned different records: %p/%p vs %p/%p", conf2, rec2, conf, rec)
	}
	if got := svc.store.Trades[trades[0].ID].DeliveredQuantity; got != 4 {
		t.Fatalf("delivered = %d, want 4 (no double decrement)", got)
	}
	if got := len(svc.store.MarginRecords); got != 1 {
		t.Fatalf("margin records = %d, want 1", got)
	}

	// 同号但数量、保证金或成交变化返回冲突。
	if _, _, err := svc.ConfirmDelivery("op-1", b.ID, trades[0].ID, 5, released(200)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("quantity change err = %v, want ErrIdempotencyConflict", err)
	}
	if _, _, err := svc.ConfirmDelivery("op-1", b.ID, trades[0].ID, 4, released(201)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("margin change err = %v, want ErrIdempotencyConflict", err)
	}
	if _, _, err := svc.ConfirmDelivery("op-1", b.ID, trades[1].ID, 4, released(200)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("trade change err = %v, want ErrIdempotencyConflict", err)
	}

	// 确认与取消共用操作号空间。
	if _, _, err := svc.CancelDelivery("op-1", b.ID, trades[1].ID, "x", forfeited(1)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("cross-type op reuse err = %v, want ErrIdempotencyConflict", err)
	}

	// 取消同样幂等。
	canc, crec, err := svc.CancelDelivery("op-c", b.ID, trades[1].ID, "buyer default", forfeited(80))
	if err != nil {
		t.Fatalf("CancelDelivery: %v", err)
	}
	if canc.CancelledQuantity != 20 {
		t.Fatalf("cancelled quantity = %d, want 20", canc.CancelledQuantity)
	}
	canc2, crec2, err := svc.CancelDelivery("op-c", b.ID, trades[1].ID, "buyer default", forfeited(80))
	if err != nil {
		t.Fatalf("cancel replay: %v", err)
	}
	if canc2 != canc || crec2 != crec {
		t.Fatalf("cancel replay returned different records")
	}
	if got := len(svc.store.MarginRecords); got != 2 {
		t.Fatalf("margin records = %d, want 2", got)
	}
	if _, _, err := svc.CancelDelivery("op-c", b.ID, trades[1].ID, "other reason", forfeited(80)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("cancel content change err = %v, want ErrIdempotencyConflict", err)
	}
}

// TestConcurrentConfirmAndClose 并发执行最后一笔确认与批次关闭：
// 无论谁先拿到锁，最终状态必须一致——确认成功后批次才能关闭，
// 关闭后的迟到确认返回明确冲突且不产生保证金记录。
func TestConcurrentConfirmAndClose(t *testing.T) {
	for i := 0; i < 50; i++ {
		svc := NewService(NewStore(""))
		trades := setupCleared(t, svc, "A1", 10)
		b := mustBatch(t, svc, "A1", trades[0])

		var wg sync.WaitGroup
		var confirmErr, closeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, confirmErr = svc.ConfirmDelivery("op-1", b.ID, trades[0].ID, 10, released(500))
		}()
		go func() {
			defer wg.Done()
			_, closeErr = svc.CloseBatch(b.ID)
		}()
		wg.Wait()

		if confirmErr != nil {
			t.Fatalf("iter %d: confirm err = %v", i, confirmErr)
		}
		// 关闭要么在确认前到达（ErrBatchNotClosable），要么在确认后成功。
		if closeErr != nil && !errors.Is(closeErr, ErrBatchNotClosable) {
			t.Fatalf("iter %d: close err = %v", i, closeErr)
		}
		if closeErr == nil {
			// 批次已关闭：迟到确认必须冲突，且保证金记录不增加。
			if _, _, err := svc.ConfirmDelivery("op-late", b.ID, trades[0].ID, 1, released(1)); !errors.Is(err, ErrBatchClosed) {
				t.Fatalf("iter %d: late confirm err = %v, want ErrBatchClosed", i, err)
			}
		} else {
			if _, err := svc.CloseBatch(b.ID); err != nil {
				t.Fatalf("iter %d: retry close err = %v", i, err)
			}
		}
		if got := len(svc.store.MarginRecords); got != 1 {
			t.Fatalf("iter %d: margin records = %d, want 1", i, got)
		}
		if got := svc.store.Trades[trades[0].ID].DeliveredQuantity; got != 10 {
			t.Fatalf("iter %d: delivered = %d, want 10", i, got)
		}
	}
}

// TestConcurrentConfirmAndCorrection 并发执行交割确认与成交更正：
// 两者依据同一结算版本裁决，确认要么先于更正生效，要么因版本过期失败。
func TestConcurrentConfirmAndCorrection(t *testing.T) {
	confirmed := 0
	for i := 0; i < 50; i++ {
		svc := NewService(NewStore(""))
		trades := setupCleared(t, svc, "A1", 10)
		b := mustBatch(t, svc, "A1", trades[0])

		var wg sync.WaitGroup
		var confirmErr, correctErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, confirmErr = svc.ConfirmDelivery("op-1", b.ID, trades[0].ID, 4, released(200))
		}()
		go func() {
			defer wg.Done()
			_, correctErr = svc.CorrectTrade(trades[0].ID, 10, 1500, "reprice")
		}()
		wg.Wait()

		if correctErr != nil {
			t.Fatalf("iter %d: correct err = %v", i, correctErr)
		}
		switch {
		case confirmErr == nil:
			confirmed++
			if got := svc.store.Trades[trades[0].ID].DeliveredQuantity; got != 4 {
				t.Fatalf("iter %d: delivered = %d, want 4", i, got)
			}
			if got := len(svc.store.MarginRecords); got != 1 {
				t.Fatalf("iter %d: margin records = %d, want 1", i, got)
			}
		case errors.Is(confirmErr, ErrVersionConflict):
			if got := len(svc.store.MarginRecords); got != 0 {
				t.Fatalf("iter %d: margin records = %d, want 0", i, got)
			}
			if got := svc.store.Trades[trades[0].ID].DeliveredQuantity; got != 0 {
				t.Fatalf("iter %d: delivered = %d, want 0", i, got)
			}
		default:
			t.Fatalf("iter %d: confirm err = %v, want nil or ErrVersionConflict", i, confirmErr)
		}

		// 无论谁先生效，更正后的确认都必须因版本过期而失败。
		if _, _, err := svc.ConfirmDelivery("op-2", b.ID, trades[0].ID, 1, released(1)); !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("iter %d: post-correction confirm err = %v, want ErrVersionConflict", i, err)
		}
	}
	if confirmed == 0 {
		t.Fatal("confirm never won the race; test is not exercising both orderings")
	}
}

func TestPersistenceRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	svc := NewService(NewStore(path))
	trades := setupCleared(t, svc, "A1", 10, 20)
	b := mustBatch(t, svc, "A1", trades...)

	conf, rec, err := svc.ConfirmDelivery("op-1", b.ID, trades[0].ID, 4, released(200))
	if err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}
	if _, err := svc.CorrectTrade(trades[1].ID, 18, 900, "fix"); err != nil {
		t.Fatalf("CorrectTrade: %v", err)
	}

	// 崩溃后从快照恢复：状态完整，幂等重放返回原结果。
	restored, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	svc2 := NewService(restored)

	view, err := svc2.GetBatchView(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Stale || view.CurrentVersion != 1 {
		t.Fatalf("restored view stale=%v current=%d", view.Stale, view.CurrentVersion)
	}
	if view.Items[0].DeliveredQuantity != 4 || view.Items[0].RemainingQuantity != 6 {
		t.Fatalf("restored item = %+v, want delivered 4 remaining 6", view.Items[0])
	}
	if len(view.Items[0].MarginRecords) != 1 || view.Items[0].MarginRecords[0].ID != rec.ID {
		t.Fatalf("restored margin records = %+v, want original %s", view.Items[0].MarginRecords, rec.ID)
	}

	conf2, rec2, err := svc2.ConfirmDelivery("op-1", b.ID, trades[0].ID, 4, released(200))
	if err != nil {
		t.Fatalf("replay after recovery: %v", err)
	}
	if conf2.Quantity != conf.Quantity || rec2.ID != rec.ID {
		t.Fatalf("replay after recovery returned %+v/%+v, want original", conf2, rec2)
	}
	if got := restored.Trades[trades[0].ID].DeliveredQuantity; got != 4 {
		t.Fatalf("delivered after replay = %d, want 4", got)
	}

	// 恢复后批次已过期，新确认按版本冲突拒绝；取消清理后可关闭。
	if _, _, err := svc2.ConfirmDelivery("op-2", b.ID, trades[0].ID, 6, released(300)); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("confirm on restored stale batch err = %v, want ErrVersionConflict", err)
	}
	if _, _, err := svc2.CancelDelivery("op-c1", b.ID, trades[0].ID, "stale", forfeited(10)); err != nil {
		t.Fatalf("CancelDelivery: %v", err)
	}
	if _, _, err := svc2.CancelDelivery("op-c2", b.ID, trades[1].ID, "stale", forfeited(20)); err != nil {
		t.Fatalf("CancelDelivery: %v", err)
	}
	if _, err := svc2.CloseBatch(b.ID); err != nil {
		t.Fatalf("CloseBatch after recovery: %v", err)
	}

	// 再次恢复，关闭状态与全部台账记录仍在。
	restored2, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if restored2.Batches[b.ID].Status != BatchClosed {
		t.Fatalf("restored batch status = %s, want CLOSED", restored2.Batches[b.ID].Status)
	}
	if got := len(restored2.MarginRecords); got != 3 {
		t.Fatalf("restored margin records = %d, want 3", got)
	}
}

func TestBatchViewQuery(t *testing.T) {
	svc := NewService(NewStore(""))
	trades := setupCleared(t, svc, "A1", 10, 20)
	b := mustBatch(t, svc, "A1", trades...)

	if _, _, err := svc.ConfirmDelivery("op-1", b.ID, trades[0].ID, 4, released(200)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.CancelDelivery("op-2", b.ID, trades[1].ID, "default", forfeited(99)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CorrectTrade(trades[0].ID, 10, 1100, "reprice"); err != nil {
		t.Fatal(err)
	}

	view, err := svc.GetBatchView(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Batch.Status != BatchOpen || !view.Stale {
		t.Fatalf("view batch status = %s stale = %v", view.Batch.Status, view.Stale)
	}
	first, second := view.Items[0], view.Items[1]
	if first.PlannedQuantity != 10 || first.DeliveredQuantity != 4 || first.RemainingQuantity != 6 || first.Status != ItemPartial {
		t.Fatalf("first item = %+v", first)
	}
	if len(first.MarginRecords) != 1 || first.MarginRecords[0].Result != released(200) {
		t.Fatalf("first margin = %+v", first.MarginRecords)
	}
	if second.Status != ItemCancelled || second.RemainingQuantity != 20 {
		t.Fatalf("second item = %+v", second)
	}
	if len(second.MarginRecords) != 1 || second.MarginRecords[0].Result != forfeited(99) {
		t.Fatalf("second margin = %+v", second.MarginRecords)
	}
	if len(view.Corrections) != 1 || view.Corrections[0].NewPriceCents != 1100 {
		t.Fatalf("corrections = %+v", view.Corrections)
	}
	if got := len(svc.ListBatches("A1")); got != 1 {
		t.Fatalf("ListBatches = %d, want 1", got)
	}
}
