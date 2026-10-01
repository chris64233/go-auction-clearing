package auctionclearing

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// httpCorrectionFixture 起好一场已清算+已生成首版结算的拍卖，返回服务地址与拍卖 ID。
func httpCorrectionFixture(t *testing.T, gw CompensationGateway) (*httptest.Server, int64) {
	t.Helper()
	clk := NewControlledClock(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	var opts []EngineOption
	if gw != nil {
		opts = append(opts, WithCompensationGateway(gw))
	}
	e, err := NewEngine(NewMemoryEventStore(), clk, opts...)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewServer(e).Handler())
	t.Cleanup(srv.Close)

	doJSON(t, "POST", srv.URL+"/v1/auctions", map[string]any{
		"available_qty": 10,
		"deadline":      "2026-09-01T10:00:00Z",
		"reserve_price": "5",
	})
	for _, b := range []map[string]any{
		{"external_id": "b1", "bidder": "A", "qty": 3, "price": "10"},
		{"external_id": "b2", "bidder": "B", "qty": 5, "price": "9"},
		{"external_id": "b3", "bidder": "C", "qty": 4, "price": "8"},
	} {
		clk.Advance(time.Second)
		doJSON(t, "POST", srv.URL+"/v1/auctions/1/bids", b)
	}
	clk.Set(time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC))
	doJSON(t, "POST", srv.URL+"/v1/auctions/1/settle", nil)
	doJSON(t, "POST", srv.URL+"/v1/auctions/1/settlements", nil)
	return srv, 1
}

func TestHTTPSettlementLifecycleAndCorrectionFullFlow(t *testing.T) {
	srv, id := httpCorrectionFixture(t, nil)

	// 首版结算重复生成：200 + replayed。
	status, body := doJSON(t, "POST", srv.URL+"/v1/auctions/1/settlements", nil)
	if status != http.StatusOK || body["outcome"] != "replayed" {
		t.Fatalf("settlement replay status=%d body=%v", status, body)
	}

	// 未确认先冻结：409 fill_not_confirmed。
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/fills/b1/freeze", nil)
	if status != http.StatusConflict || body["code"] != "fill_not_confirmed" {
		t.Fatalf("freeze order status=%d body=%v", status, body)
	}

	// 确认 → 冻结 → 交割 b1 3 件（8*3=24）。
	if status, _ = doJSON(t, "POST", srv.URL+"/v1/auctions/1/fills/b1/confirm", nil); status != http.StatusOK {
		t.Fatalf("confirm status=%d", status)
	}
	doJSON(t, "POST", srv.URL+"/v1/auctions/1/fills/b1/freeze", nil)
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/fills/b1/deliver", map[string]any{"quantity": 3})
	if status != http.StatusOK || body["delivered_qty"].(float64) != 3 ||
		body["delivered_amount"] != "24" || body["status"] != "delivered" {
		t.Fatalf("deliver body=%v", body)
	}

	// 超量交割：409。
	if status, _ = doJSON(t, "POST", srv.URL+"/v1/auctions/1/fills/b1/deliver", map[string]any{"quantity": 1}); status != http.StatusConflict {
		t.Fatalf("overdeliver status=%d", status)
	}

	// 提交更正（已交割 b1 改 6；未交割 b2 改 5）。
	corrReq := map[string]any{
		"correction_id": "COR-HTTP-1",
		"reason":        "wrong feed price",
		"basis":         "feed-v2-reconciliation",
		"handler":       "risk-officer-1",
		"items": []map[string]any{
			{"external_id": "b1", "target_unit_price": "6"},
			{"external_id": "b2", "target_unit_price": "5"},
		},
	}
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/corrections", corrReq)
	if status != http.StatusCreated || body["outcome"] != "accepted" {
		t.Fatalf("submit correction status=%d body=%v", status, body)
	}
	// 幂等重放：200。
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/corrections", corrReq)
	if status != http.StatusOK || body["outcome"] != "replayed" {
		t.Fatalf("replay correction status=%d body=%v", status, body)
	}
	// 内容变化：409 correction_conflict。
	bad := corrReq
	bad["reason"] = "changed"
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/corrections", bad)
	if status != http.StatusConflict || body["code"] != "correction_conflict" {
		t.Fatalf("conflict status=%d body=%v", status, body)
	}

	// 批准。
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/corrections/COR-HTTP-1/approve", nil)
	if status != http.StatusCreated || body["outcome"] != "accepted" {
		t.Fatalf("approve status=%d body=%v", status, body)
	}
	data := body["data"].(map[string]any)
	if data["status"] != "completed" {
		t.Fatalf("data=%v", data)
	}
	comps := data["compensations"].([]any)
	if len(comps) != 1 || comps[0].(map[string]any)["amount"] != "6" {
		t.Fatalf("compensations=%v", comps)
	}
	// 重复批准：200 replayed。
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/corrections/COR-HTTP-1/approve", nil)
	if status != http.StatusOK || body["outcome"] != "replayed" {
		t.Fatalf("replay approve status=%d body=%v", status, body)
	}

	// 当前结算：v2，b1 原价保留+交割，b2 新价。
	status, body = doJSON(t, "GET", srv.URL+"/v1/auctions/1/settlements", nil)
	if status != http.StatusOK || body["version"].(float64) != 2 {
		t.Fatalf("settlement status=%d body=%v", status, body)
	}
	entries := body["entries"].([]any)
	got := map[string]map[string]any{}
	for _, en := range entries {
		m := en.(map[string]any)
		got[m["external_id"].(string)] = m
	}
	if got["b1"]["effective_unit_price"] != "8" || got["b1"]["settled_amount"] != "24" {
		t.Fatalf("b1=%v", got["b1"])
	}
	if got["b2"]["effective_unit_price"] != "5" || got["b2"]["settled_amount"] != "25" {
		t.Fatalf("b2=%v", got["b2"])
	}

	// 列表与单查。
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/auctions/1/corrections", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var listed []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(listed) != 1 {
		t.Fatalf("list status=%d items=%d", resp.StatusCode, len(listed))
	}
	status, body = doJSON(t, "GET", srv.URL+"/v1/auctions/1/corrections/COR-HTTP-1", nil)
	if status != http.StatusOK || body["correction_id"] != "COR-HTTP-1" {
		t.Fatalf("get correction status=%d body=%v", status, body)
	}

	// 追溯：b1=delivered_compensated，b2=undelivered。
	status, body = doJSON(t, "GET", srv.URL+"/v1/auctions/1/corrections/COR-HTTP-1/trace", nil)
	if status != http.StatusOK {
		t.Fatalf("trace status=%d body=%v", status, body)
	}
	for _, it := range body["items"].([]any) {
		m := it.(map[string]any)
		switch m["external_id"] {
		case "b1":
			if m["scenario"] != "delivered_compensated" || m["compensation"] == nil {
				t.Fatalf("trace b1=%v", m)
			}
		case "b2":
			if m["scenario"] != "undelivered" || m["compensation"] != nil {
				t.Fatalf("trace b2=%v", m)
			}
		}
	}

	// 不存在的更正：404。
	if status, _ = doJSON(t, "GET", srv.URL+"/v1/auctions/1/corrections/nope", nil); status != http.StatusNotFound {
		t.Fatalf("missing correction status=%d", status)
	}
	_ = id
}

// 补偿失败 → pending → 重试成功的完整 HTTP 链路。
func TestHTTPCorrectionCompensationFailureAndRetry(t *testing.T) {
	gw := newFailingGateway(true)
	srv, _ := httpCorrectionFixture(t, gw)
	doJSON(t, "POST", srv.URL+"/v1/auctions/1/fills/b1/confirm", nil)
	doJSON(t, "POST", srv.URL+"/v1/auctions/1/fills/b1/deliver", map[string]any{"quantity": 3})

	raw := map[string]any{
		"correction_id": "COR-HTTP-FAIL",
		"reason":        "r", "basis": "b", "handler": "h",
		"items": []map[string]any{{"external_id": "b1", "target_unit_price": "6"}},
	}
	doJSON(t, "POST", srv.URL+"/v1/auctions/1/corrections", raw)
	status, body := doJSON(t, "POST", srv.URL+"/v1/auctions/1/corrections/COR-HTTP-FAIL/approve", nil)
	if status != http.StatusCreated {
		t.Fatalf("approve status=%d", status)
	}
	if body["data"].(map[string]any)["status"] != "pending" {
		t.Fatalf("approve body=%v", body)
	}
	// 结算仍停在 v1，原成交可查。
	status, body = doJSON(t, "GET", srv.URL+"/v1/auctions/1/settlements", nil)
	if body["version"].(float64) != 1 {
		t.Fatalf("version moved on failure: %v", body)
	}
	// 对 completed 更正重试补偿：409 correction_not_pending（先验证错误码存在）。
	doJSON(t, "POST", srv.URL+"/v1/auctions/1/corrections", map[string]any{
		"correction_id": "COR-HTTP-OK", "reason": "r", "basis": "b", "handler": "h",
		"items": []map[string]any{{"external_id": "b2", "target_unit_price": "8"}}, // 无变化
	})
	doJSON(t, "POST", srv.URL+"/v1/auctions/1/corrections/COR-HTTP-OK/approve", nil)
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/corrections/COR-HTTP-OK/retry-compensation", nil)
	if status != http.StatusConflict || body["code"] != "correction_not_pending" {
		t.Fatalf("retry completed status=%d body=%v", status, body)
	}

	// 通道恢复后重试成功。
	gw.setFail(false)
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/corrections/COR-HTTP-FAIL/retry-compensation", nil)
	if status != http.StatusOK || body["status"] != "completed" || body["effective_version"].(float64) != 2 {
		t.Fatalf("retry status=%d body=%v", status, body)
	}
}

// 金额格式错误返回 400，且不产生副作用。
func TestHTTPCorrectionInvalidMoney(t *testing.T) {
	srv, _ := httpCorrectionFixture(t, nil)
	buf := bytes.NewReader([]byte(`{"correction_id":"C","reason":"r","basis":"b","handler":"h","items":[{"external_id":"b1","target_unit_price":"abc"}]}`))
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/auctions/1/corrections", buf)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var eb struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &eb); err != nil || eb.Code != "invalid_argument" {
		t.Fatalf("body=%s err=%v", raw, err)
	}
}
