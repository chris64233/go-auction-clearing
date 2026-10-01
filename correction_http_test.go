package auctionclearing

import (
	"net/http"
	"testing"
	"time"
)

// httpClearedAuction 经 HTTP 造一场已清算的拍卖并返回 id。
func httpClearedAuction(t *testing.T, h http.Handler, clock *fakeClock) string {
	t.Helper()
	deadline := clock.Now().Add(time.Hour).Format(time.RFC3339)
	status, body := doJSON(t, h, "POST", "/api/v1/auctions", map[string]any{
		"available_qty": 10, "deadline": deadline, "reserve_price": "5",
	})
	if status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, body)
	}
	id := body["id"].(string)
	for _, p := range []map[string]any{
		{"external_ref": "ORD-A", "bidder": "alice", "quantity": 3, "unit_price": "10"},
		{"external_ref": "ORD-B", "bidder": "bob", "quantity": 2, "unit_price": "8"},
	} {
		if s, b := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/bids", p); s != http.StatusCreated {
			t.Fatalf("bid: %d %v", s, b)
		}
	}
	clock.Advance(2 * time.Hour)
	if s, b := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/clear", nil); s != http.StatusCreated {
		t.Fatalf("clear: %d %v", s, b)
	}
	return id
}

func TestHTTPCorrectionFullFlow(t *testing.T) {
	h, _, clock := newTestServer(t)
	id := httpClearedAuction(t, h, clock)

	// 生成结算：首次 201，重复 200 + replayed。
	if s, b := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/settle", nil); s != http.StatusCreated {
		t.Fatalf("settle: %d %v", s, b)
	}
	if s, b := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/settle", nil); s != http.StatusOK || b["replayed"] != true {
		t.Fatalf("settle replay: %d %v", s, b)
	}

	// 成交确认 → 冻结。
	if s, b := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/fills/ORD-B/confirm", nil); s != http.StatusOK {
		t.Fatalf("confirm: %d %v", s, b)
	}
	if s, b := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/fills/ORD-B/freeze", nil); s != http.StatusOK || b["funds_frozen"] != true {
		t.Fatalf("freeze: %d %v", s, b)
	}
	// 未确认先冻结 422。
	if s, _ := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/fills/ORD-A/freeze", nil); s != http.StatusUnprocessableEntity {
		t.Fatalf("freeze-before-confirm status = %d", s)
	}

	// 提交更正。
	corrReq := map[string]any{
		"correction_id": "COR-1",
		"reason":        "wrong feed price",
		"basis":         "feed-recon-1",
		"handler":       "officer-1",
		"items": []map[string]any{
			{"external_ref": "ORD-A", "target_unit_price": "9"},
			{"external_ref": "ORD-B", "target_unit_price": "7"},
		},
	}
	if s, b := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/corrections", corrReq); s != http.StatusCreated {
		t.Fatalf("submit: %d %v", s, b)
	}
	// 幂等重放 200。
	if s, b := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/corrections", corrReq); s != http.StatusOK || b["replayed"] != true {
		t.Fatalf("submit replay: %d %v", s, b)
	}
	// 同号改价 409。
	conflictReq := map[string]any{
		"correction_id": "COR-1",
		"reason":        "wrong feed price",
		"basis":         "feed-recon-1",
		"handler":       "officer-1",
		"items":         []map[string]any{{"external_ref": "ORD-A", "target_unit_price": "8"}},
	}
	conflictReq["items"] = []map[string]any{{"external_ref": "ORD-A", "target_unit_price": "8"}}
	if s, _ := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/corrections", conflictReq); s != http.StatusConflict {
		t.Fatalf("conflict status = %d", s)
	}
	// 缺依据 400。
	bad := map[string]any{
		"correction_id": "COR-2",
		"reason":        "wrong feed price",
		"handler":       "officer-1",
		"items":         []map[string]any{{"external_ref": "ORD-A", "target_unit_price": "9"}},
	}
	bad["correction_id"] = "COR-2"
	bad["basis"] = ""
	if s, _ := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/corrections", bad); s != http.StatusBadRequest {
		t.Fatalf("missing basis status = %d", s)
	}

	// 批准：201。
	if s, b := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/corrections/COR-1/approve",
		map[string]any{"handler": "officer-2", "basis": "approval-note-9"}); s != http.StatusCreated {
		t.Fatalf("approve: %d %v", s, b)
	}
	// 重复批准 200 + replayed。
	if s, b := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/corrections/COR-1/approve", nil); s != http.StatusOK || b["replayed"] != true {
		t.Fatalf("approve replay: %d %v", s, b)
	}

	// 当前结算版本为 v2，ORD-A 改价 3*9=27，ORD-B 改价 2*7=14。
	s, b := doJSON(t, h, "GET", "/api/v1/auctions/"+id+"/settlement", nil)
	if s != http.StatusOK {
		t.Fatalf("get settlement: %d %v", s, b)
	}
	entries := b["entries"].([]any)
	first := entries[0].(map[string]any)
	if b["version"].(float64) != 2 || first["effective_unit_price"] != "9" {
		t.Fatalf("settlement v2 mismatch: %v", b)
	}

	// 追溯查询：ORD-A 未交割 → 无补偿；ORD-B 未交割 → 无补偿。
	s, b = doJSON(t, h, "GET", "/api/v1/auctions/"+id+"/corrections/COR-1/trace", nil)
	if s != http.StatusOK {
		t.Fatalf("trace: %d %v", s, b)
	}
	if b["reason"] != "wrong feed price" || b["handler"] != "officer-1" {
		t.Fatalf("trace header: %v", b)
	}
	items := b["items"].([]any)
	for _, it := range items {
		m := it.(map[string]any)
		if m["scenario"] != "undelivered" || m["compensation"] != nil {
			t.Fatalf("undelivered trace item: %v", m)
		}
		if m["original"].(map[string]any)["unit_price"] == nil {
			t.Fatalf("original fill missing: %v", m)
		}
	}

	// 列表与单查。
	if s, b := doJSON(t, h, "GET", "/api/v1/auctions/"+id+"/corrections", nil); s != http.StatusOK ||
		len(b["corrections"].([]any)) != 1 {
		t.Fatalf("list: %d %v", s, b)
	}
	if s, _ := doJSON(t, h, "GET", "/api/v1/auctions/"+id+"/corrections/missing", nil); s != http.StatusNotFound {
		t.Fatalf("missing correction status = %d", s)
	}
}

func TestHTTPDeliveredCompensationFlow(t *testing.T) {
	h, _, clock := newTestServer(t)
	id := httpClearedAuction(t, h, clock)
	if s, b := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/settle", nil); s != http.StatusCreated {
		t.Fatalf("settle: %d %v", s, b)
	}
	// 交割 ORD-A 3 件。
	if s, b := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/fills/ORD-A/deliver",
		map[string]any{"quantity": 3}); s != http.StatusOK || b["delivered_qty"].(float64) != 3 {
		t.Fatalf("deliver: %d %v", s, b)
	}
	corrReq := map[string]any{
		"correction_id": "COR-D", "reason": "r", "basis": "b", "handler": "h",
		"items": []map[string]any{{"external_ref": "ORD-A", "target_unit_price": "9"}},
	}
	if s, b := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/corrections", corrReq); s != http.StatusCreated {
		t.Fatalf("submit: %d %v", s, b)
	}
	if s, b := doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/corrections/COR-D/approve", nil); s != http.StatusCreated {
		t.Fatalf("approve: %d %v", s, b)
	}
	s, b := doJSON(t, h, "GET", "/api/v1/auctions/"+id+"/corrections/COR-D/trace", nil)
	if s != http.StatusOK {
		t.Fatalf("trace: %d %v", s, b)
	}
	item := b["items"].([]any)[0].(map[string]any)
	if item["scenario"] != "delivered_compensated" {
		t.Fatalf("scenario = %v", item["scenario"])
	}
	comp := item["compensation"].(map[string]any)
	if comp["amount"] != "3" || comp["status"] != "settled" {
		t.Fatalf("compensation: %v", comp)
	}
}
