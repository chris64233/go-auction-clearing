package auctionclearing

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// FileEventStore 持久化 + 引擎重启重放。
func TestFileEventStorePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	clk := NewControlledClock(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	func() {
		store, err := NewFileEventStore(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		e, err := NewEngine(store, clk)
		if err != nil {
			t.Fatal(err)
		}
		dl := clk.Now().Add(time.Hour)
		if _, err := e.CreateAuction(8, dl, MustParseMoney("5")); err != nil {
			t.Fatal(err)
		}
		if _, err := e.SubmitBid(BidInput{AuctionID: 1, ExternalID: "b1", Bidder: "A", Qty: 5, Price: MustParseMoney("10")}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.SubmitBid(BidInput{AuctionID: 1, ExternalID: "b2", Bidder: "B", Qty: 6, Price: MustParseMoney("9")}); err != nil {
			t.Fatal(err)
		}
		clk.Set(dl)
		if _, err := e.SettleAuction(1); err != nil {
			t.Fatal(err)
		}
	}()

	// 重新打开同一日志文件：状态完整恢复，且重复清算不会二次成交。
	func() {
		store, err := NewFileEventStore(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		e, err := NewEngine(store, clk)
		if err != nil {
			t.Fatal(err)
		}
		s, err := e.GetSettlement(1)
		if err != nil {
			t.Fatal(err)
		}
		if s.SoldQty != 8 || s.ClearingPrice.String() != "9" || s.TotalProceeds.String() != "72" {
			t.Fatalf("reloaded settlement = %+v", s)
		}
		r, err := e.SettleAuction(1)
		if err != nil || r.Outcome != SettleReplayed {
			t.Fatalf("settle after reload: %+v err=%v", r, err)
		}
	}()

	// 事件日志每行一个 JSON，且只应有一个 settled 事件。
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("event log is empty")
	}
	store2, err := NewFileEventStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	events, err := store2.Load()
	if err != nil {
		t.Fatal(err)
	}
	settledCount := 0
	var prevSeq int64
	for i, ev := range events {
		if ev.Seq != prevSeq+1 {
			t.Fatalf("seq gap at event %d: %d after %d", i, ev.Seq, prevSeq)
		}
		prevSeq = ev.Seq
		if ev.Type == EvSettled {
			settledCount++
		}
	}
	if settledCount != 1 {
		t.Fatalf("settled events = %d, want exactly 1", settledCount)
	}
}

// 缺失文件自动创建；空日志可正常加载。
func TestFileEventStoreEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	store, err := NewFileEventStore(path)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("empty log loaded %d events", len(events))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// 日志最后一行残缺（模拟写到一半崩溃）：加载阶段明确报错，
// 引擎不会在半截事实上构建状态。
func TestFileEventStoreCorruptedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	good := `{"seq":1,"type":"auction_created","timestamp":"2026-09-01T09:00:00Z","auction_id":1,"available_qty":10,"deadline":"2026-09-01T10:00:00Z","reserve_price":"0"}` + "\n"
	if err := os.WriteFile(path, []byte(good+`{"seq":2,"type":"bi`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileEventStore(path); err == nil {
		t.Fatal("expected error loading corrupted log")
	}
}

// 事件序号断档：加载阶段明确报错。
func TestFileEventStoreSequenceGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	line := func(seq int64) string {
		return `{"seq":` + strconv.FormatInt(seq, 10) +
			`,"type":"auction_created","timestamp":"2026-09-01T09:00:00Z","auction_id":1,"available_qty":10,"deadline":"2026-09-01T10:00:00Z","reserve_price":"0"}` + "\n"
	}
	if err := os.WriteFile(path, []byte(line(1)+line(3)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileEventStore(path); err == nil {
		t.Fatal("expected error on sequence gap")
	}
}
