package auctionclearing

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newClearedTrade(t *testing.T, s *Service, id string, qty, price int64) {
	t.Helper()
	if _, err := s.RegisterTrade(id, "auc-1", "buyer", "seller", qty, price); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	if err := s.ClearTrade(id); err != nil {
		t.Fatalf("clear %s: %v", id, err)
	}
}

func TestCreateBatchFreezesAndGuardsMembership(t *testing.T) {
	s := NewService(NewStore(""))
	newClearedTrade(t, s, "t1", 10, 100)
	newClearedTrade(t, s, "t2", 5, 200)

	b, err := s.CreateBatch("auc-1", []string{"t1", "t2"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	if b.Items[0].PlannedQuantity != 10 || b.Items[0].SettlementVersion != 1 || b.Items[0].Price != 100 {
		t.Fatalf("frozen snapshot wrong: %+v", b.Items[0])
	}
	// 一笔成交只能属于一个未完成批次
	if _, err := s.CreateBatch("auc-1", []string{"t1"}, time.Now()); !errors.Is(err, ErrTradeAlreadyInBatch) {
		t.Fatalf("want ErrTradeAlreadyInBatch, got %v", err)
	}
	// 未清算成交不能入批
	if _, err := s.RegisterTrade("t3", "auc-1", "b", "s", 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("auc-1", []string{"t3"}, time.Now()); !errors.Is(err, ErrTradeNotCleared) {
		t.Fatalf("want ErrTradeNotCleared, got %v", err)
	}
	// 已交割成交不能再次加入新批次
	if _, err := s.ConfirmDelivery("op-t2", b.ID, "t2", 5, MarginActionRelease, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBatch("auc-1", []string{"t2"}, time.Now()); !errors.Is(err, ErrTradeAlreadyDelivered) {
		t.Fatalf("want ErrTradeAlreadyDelivered, got %v", err)
	}
}

func TestPartialDeliveryAndClose(t *testing.T) {
	s := NewService(NewStore(""))
	newClearedTrade(t, s, "t1", 10, 100)
	b, err := s.CreateBatch("auc-1", []string{"t1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// 部分交割 4,剩余 6,批次不可关闭
	if _, err := s.ConfirmDelivery("op-1", b.ID, "t1", 4, MarginActionRelease, 400); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseBatch(b.ID); !errors.Is(err, ErrBatchNotClosable) {
		t.Fatalf("want ErrBatchNotClosable, got %v", err)
	}
	// 超量交割被拒绝:不能把未交割部分伪装成完成
	if _, err := s.ConfirmDelivery("op-2", b.ID, "t1", 7, MarginActionRelease, 700); !errors.Is(err, ErrOverDelivery) {
		t.Fatalf("want ErrOverDelivery, got %v", err)
	}
	if _, err := s.ConfirmDelivery("op-3", b.ID, "t1", 6, MarginActionRelease, 600); err != nil {
		t.Fatal(err)
	}
	view, err := s.GetBatchView(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	it := view.Items[0]
	if it.DeliveredQuantity != 10 || it.RemainingQuantity != 0 || it.Status != ItemStatusCompleted {
		t.Fatalf("bad item view: %+v", it)
	}
	if len(it.Margins) != 2 || it.Margins[0].Amount != 400 || it.Margins[1].Amount != 600 {
		t.Fatalf("bad margins: %+v", it.Margins)
	}
	if err := s.CloseBatch(b.ID); err != nil {
		t.Fatalf("close: %v", err)
	}
	// 批次关闭后的迟到确认返回明确冲突
	if _, err := s.ConfirmDelivery("op-late", b.ID, "t1", 1, MarginActionRelease, 100); !errors.Is(err, ErrBatchClosed) {
		t.Fatalf("want ErrBatchClosed, got %v", err)
	}
}

func TestCorrectionInvalidatesOpenBatch(t *testing.T) {
	s := NewService(NewStore(""))
	newClearedTrade(t, s, "t1", 10, 100)
	b, err := s.CreateBatch("auc-1", []string{"t1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// 更正先生效
	if _, err := s.CorrectTrade("t1", 8, 120); err != nil {
		t.Fatal(err)
	}
	// 旧批次不得继续确认
	if _, err := s.ConfirmDelivery("op-1", b.ID, "t1", 5, MarginActionRelease, 500); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("want ErrVersionConflict, got %v", err)
	}
	// 查询可见冻结版本与当前版本差异
	view, _ := s.GetBatchView(b.ID)
	if view.Items[0].SettlementVersion != 1 || view.Items[0].CurrentVersion != 2 {
		t.Fatalf("version view wrong: %+v", view.Items[0])
	}
	// 取消后可按新版本重新入批
	if err := s.CancelBatchItem(b.ID, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseBatch(b.ID); err != nil {
		t.Fatal(err)
	}
	b2, err := s.CreateBatch("auc-1", []string{"t1"}, time.Now())
	if err != nil {
		t.Fatalf("re-batch after cancel: %v", err)
	}
	if b2.Items[0].SettlementVersion != 2 || b2.Items[0].PlannedQuantity != 8 {
		t.Fatalf("re-batch snapshot wrong: %+v", b2.Items[0])
	}
}

func TestConcurrentConfirmAndClose(t *testing.T) {
	for i := 0; i < 50; i++ {
		s := NewService(NewStore(""))
		newClearedTrade(t, s, "t1", 10, 100)
		b, err := s.CreateBatch("auc-1", []string{"t1"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var confirmErr, closeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, confirmErr = s.ConfirmDelivery("op-x", b.ID, "t1", 10, MarginActionRelease, 1000)
		}()
		go func() {
			defer wg.Done()
			closeErr = s.CloseBatch(b.ID)
		}()
		wg.Wait()
		// 裁决结果必须一致,不允许出现孤立的交割/保证金记录
		view, _ := s.GetBatchView(b.ID)
		if confirmErr == nil {
			if view.Items[0].DeliveredQuantity != 10 || len(view.Items[0].Margins) != 1 {
				t.Fatalf("iter %d: inconsistent state: %+v", i, view.Items[0])
			}
			// 确认先生效时关闭可能被拒(尚有未完结项),但重试关闭必须成功
			if closeErr != nil && !errors.Is(closeErr, ErrBatchNotClosable) {
				t.Fatalf("iter %d: unexpected close err %v", i, closeErr)
			}
			if err := s.CloseBatch(b.ID); err != nil {
				t.Fatalf("iter %d: retry close: %v", i, err)
			}
		} else {
			// 批次先关闭时迟到确认必须返回明确冲突
			if !errors.Is(confirmErr, ErrBatchClosed) {
				t.Fatalf("iter %d: unexpected confirm err %v", i, confirmErr)
			}
			if closeErr != nil {
				t.Fatalf("iter %d: close failed: %v", i, closeErr)
			}
			if view.Items[0].DeliveredQuantity != 0 || len(view.Items[0].Margins) != 0 {
				t.Fatalf("iter %d: orphan margin/delivery: %+v", i, view.Items[0])
			}
		}
	}
}

func TestIdempotentReplay(t *testing.T) {
	s := NewService(NewStore(""))
	newClearedTrade(t, s, "t1", 10, 100)
	b, err := s.CreateBatch("auc-1", []string{"t1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c1, err := s.ConfirmDelivery("op-1", b.ID, "t1", 4, MarginActionRelease, 400)
	if err != nil {
		t.Fatal(err)
	}
	// 同号同内容重放返回原结果,不重复扣减
	c2, err := s.ConfirmDelivery("op-1", b.ID, "t1", 4, MarginActionRelease, 400)
	if err != nil {
		t.Fatal(err)
	}
	if c1.ID != c2.ID {
		t.Fatalf("replay should return original confirmation %s, got %s", c1.ID, c2.ID)
	}
	view, _ := s.GetBatchView(b.ID)
	if view.Items[0].DeliveredQuantity != 4 || len(view.Items[0].Margins) != 1 {
		t.Fatalf("replay duplicated delivery: %+v", view.Items[0])
	}
	// 同号不同内容返回冲突
	if _, err := s.ConfirmDelivery("op-1", b.ID, "t1", 5, MarginActionRelease, 400); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("want ErrIdempotencyConflict, got %v", err)
	}
	if _, err := s.ConfirmDelivery("op-1", b.ID, "t1", 4, MarginActionForfeit, 400); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("margin change should conflict, got %v", err)
	}
}

func TestPersistenceRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	s := NewService(NewStore(path))
	newClearedTrade(t, s, "t1", 10, 100)
	b, err := s.CreateBatch("auc-1", []string{"t1"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmDelivery("op-1", b.ID, "t1", 4, MarginActionRelease, 400); err != nil {
		t.Fatal(err)
	}

	// 模拟重启:从快照恢复
	store, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s2 := NewService(store)
	view, err := s2.GetBatchView(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Items[0].DeliveredQuantity != 4 || view.Items[0].RemainingQuantity != 6 {
		t.Fatalf("recovered state wrong: %+v", view.Items[0])
	}
	// 恢复后幂等重放仍返回原结果
	c, err := s2.ConfirmDelivery("op-1", b.ID, "t1", 4, MarginActionRelease, 400)
	if err != nil {
		t.Fatal(err)
	}
	if c.Quantity != 4 {
		t.Fatalf("recovered replay wrong: %+v", c)
	}
	// 恢复后继续交割不重复扣减
	if _, err := s2.ConfirmDelivery("op-2", b.ID, "t1", 6, MarginActionRelease, 600); err != nil {
		t.Fatal(err)
	}
	view, _ = s2.GetBatchView(b.ID)
	if view.Items[0].DeliveredQuantity != 10 || view.Items[0].Status != ItemStatusCompleted {
		t.Fatalf("post-recovery delivery wrong: %+v", view.Items[0])
	}
}
