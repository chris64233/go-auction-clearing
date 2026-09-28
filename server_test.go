package auctionclearing

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, *Service, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	svc := NewService(NewMemoryRepository(), clock)
	return NewServer(svc), svc, clock
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(data)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode response %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, decoded
}

func TestHTTPCreateToClearFlow(t *testing.T) {
	h, svc, clock := newTestServer(t)
	deadline := clock.Now().Add(time.Hour).Format(time.RFC3339)

	status, body := doJSON(t, h, "POST", "/api/v1/auctions", map[string]any{
		"available_qty": 25,
		"deadline":      deadline,
		"reserve_price": "5",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status = %d body = %v", status, body)
	}
	id := body["id"].(string)

	// 报价：首次 201。
	status, body = doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/bids", map[string]any{
		"external_ref": "ORD-1", "bidder": "alice", "quantity": 20, "unit_price": "10.00",
	})
	if status != http.StatusCreated {
		t.Fatalf("bid status = %d body = %v", status, body)
	}
	// 查询单份报价。
	status, body = doJSON(t, h, "GET", "/api/v1/auctions/"+id+"/bids/ORD-1", nil)
	if status != http.StatusOK || body["state"] != "active" || body["unit_price"] != "10" {
		t.Fatalf("get bid status = %d body = %v", status, body)
	}
	status, _ = doJSON(t, h, "GET", "/api/v1/auctions/"+id+"/bids/missing", nil)
	if status != http.StatusNotFound {
		t.Fatalf("get missing bid status = %d", status)
	}
	// 幂等重放：200 且 replayed=true。
	status, body = doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/bids", map[string]any{
		"external_ref": "ORD-1", "bidder": "alice", "quantity": 20, "unit_price": "10.00",
	})
	if status != http.StatusOK || body["replayed"] != true {
		t.Fatalf("replay status = %d body = %v", status, body)
	}
	// 内容冲突：409。
	status, body = doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/bids", map[string]any{
		"external_ref": "ORD-1", "bidder": "alice", "quantity": 21, "unit_price": "10.00",
	})
	if status != http.StatusConflict {
		t.Fatalf("conflict status = %d body = %v", status, body)
	}
	if body["error"].(map[string]any)["kind"] != "conflict" {
		t.Fatalf("error kind = %v", body["error"])
	}
	// 参数错误：400。
	status, _ = doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/bids", map[string]any{
		"external_ref": "ORD-X", "bidder": "bob", "quantity": 0, "unit_price": "10",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("validation status = %d", status)
	}

	// 第二份更高价的报价，随后撤回。
	_, _ = doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/bids", map[string]any{
		"external_ref": "ORD-2", "bidder": "carol", "quantity": 10, "unit_price": "20",
	})
	status, body = doJSON(t, h, "DELETE", "/api/v1/auctions/"+id+"/bids/ORD-2", nil)
	if status != http.StatusOK || body["bid"].(map[string]any)["state"] != "withdrawn" {
		t.Fatalf("withdraw status = %d body = %v", status, body)
	}

	// 未截止清算：422。
	status, body = doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/clear", nil)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("early clear status = %d body = %v", status, body)
	}
	// 查询尚不存在的结果：404。
	status, _ = doJSON(t, h, "GET", "/api/v1/auctions/"+id+"/result", nil)
	if status != http.StatusNotFound {
		t.Fatalf("result-before-clear status = %d", status)
	}

	// 推进到截止后清算。
	_ = svc
	clock.Advance(2 * time.Hour)
	status, body = doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/clear", nil)
	if status != http.StatusCreated {
		t.Fatalf("clear status = %d body = %v", status, body)
	}
	result := body["result"].(map[string]any)
	if result["sold_qty"].(float64) != 20 {
		t.Fatalf("sold = %v, want 20 (withdrawn ORD-2 excluded)", result["sold_qty"])
	}
	// 重复清算：200 + replayed。
	status, body = doJSON(t, h, "POST", "/api/v1/auctions/"+id+"/clear", nil)
	if status != http.StatusOK || body["replayed"] != true {
		t.Fatalf("replay clear status = %d body = %v", status, body)
	}
	// 清算后查询结果。
	status, body = doJSON(t, h, "GET", "/api/v1/auctions/"+id+"/result", nil)
	if status != http.StatusOK || body["sold_qty"].(float64) != 20 {
		t.Fatalf("get result status = %d body = %v", status, body)
	}
	// 拍卖不存在：404。
	status, _ = doJSON(t, h, "GET", "/api/v1/auctions/does-not-exist", nil)
	if status != http.StatusNotFound {
		t.Fatalf("missing auction status = %d", status)
	}
}

func TestHTTPBadJSONAndMoneyString(t *testing.T) {
	h, _, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/api/v1/auctions", strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json status = %d", rec.Code)
	}
	// 金额必须是字符串。
	body := `{"available_qty":1,"deadline":"2026-01-01T12:00:00Z","reserve_price":1.5}`
	req = httptest.NewRequest("POST", "/api/v1/auctions", strings.NewReader(body))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("numeric money status = %d, want 400", rec.Code)
	}
}
