package auctionclearing

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*httptest.Server, *ControlledClock) {
	t.Helper()
	clk := NewControlledClock(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))
	e, err := NewEngine(NewMemoryEventStore(), clk)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(NewServer(e).Handler()), clk
}

func doJSON(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode %s: %v: %s", url, err, raw)
		}
	}
	return resp.StatusCode, out
}

func TestHTTPCreateAndGetAuction(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()

	status, body := doJSON(t, "POST", srv.URL+"/v1/auctions", map[string]any{
		"available_qty": 10,
		"deadline":      "2026-09-01T10:00:00Z",
		"reserve_price": "5.00",
	})
	if status != http.StatusCreated {
		t.Fatalf("create status=%d body=%v", status, body)
	}
	if body["id"].(float64) != 1 || body["status"] != "open" || body["reserve_price"] != "5" {
		t.Fatalf("unexpected create body: %v", body)
	}

	status, body = doJSON(t, "GET", srv.URL+"/v1/auctions/1", nil)
	if status != http.StatusOK || body["available_qty"].(float64) != 10 {
		t.Fatalf("get status=%d body=%v", status, body)
	}

	// 参数错误。
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions", map[string]any{
		"available_qty": 0,
		"deadline":      "2026-09-01T10:00:00Z",
		"reserve_price": "0",
	})
	if status != http.StatusBadRequest || body["code"] != "invalid_argument" {
		t.Fatalf("validation status=%d body=%v", status, body)
	}
	// 不存在。
	status, _ = doJSON(t, "GET", srv.URL+"/v1/auctions/999", nil)
	if status != http.StatusNotFound {
		t.Fatalf("missing auction status=%d", status)
	}
	// 路径参数非法。
	status, _ = doJSON(t, "GET", srv.URL+"/v1/auctions/abc", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("bad path id status=%d", status)
	}
}

func TestHTTPBidIdempotencyConflictAndSettle(t *testing.T) {
	srv, clk := newTestServer(t)
	defer srv.Close()

	_, _ = doJSON(t, "POST", srv.URL+"/v1/auctions", map[string]any{
		"available_qty": 10,
		"deadline":      "2026-09-01T10:00:00Z",
		"reserve_price": "0",
	})

	bid := map[string]any{"external_id": "b1", "bidder": "A", "qty": 4, "price": "10.00"}
	status, body := doJSON(t, "POST", srv.URL+"/v1/auctions/1/bids", bid)
	if status != http.StatusCreated || body["outcome"] != "accepted" {
		t.Fatalf("first bid status=%d body=%v", status, body)
	}
	// 相同内容重复提交：200 replayed。
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/bids", bid)
	if status != http.StatusOK || body["outcome"] != "replayed" {
		t.Fatalf("replay bid status=%d body=%v", status, body)
	}
	// 不同内容：409 bid_conflict。
	conflict := map[string]any{"external_id": "b1", "bidder": "A", "qty": 9, "price": "10.00"}
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/bids", conflict)
	if status != http.StatusConflict || body["code"] != "bid_conflict" {
		t.Fatalf("conflict status=%d body=%v", status, body)
	}
	// 金额必须是字符串：浮点数字面量被拒绝。
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/bids", map[string]any{
		"external_id": "b2", "bidder": "B", "qty": 1, "price": 10.5,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("numeric price status=%d body=%v", status, body)
	}

	// 第二份报价：边界将部分成交。
	_, _ = doJSON(t, "POST", srv.URL+"/v1/auctions/1/bids", map[string]any{
		"external_id": "b2", "bidder": "B", "qty": 10, "price": "9.00",
	})

	// 撤回 b2 后再幂等撤回。
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/bids/b2/withdraw", nil)
	if status != http.StatusOK || body["outcome"] != "withdrawn" {
		t.Fatalf("withdraw status=%d body=%v", status, body)
	}
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/bids/b2/withdraw", nil)
	if status != http.StatusOK || body["outcome"] != "replayed" {
		t.Fatalf("withdraw replay status=%d body=%v", status, body)
	}

	// 截止前清算：409。
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/settle", nil)
	if status != http.StatusConflict || body["code"] != "auction_not_closed" {
		t.Fatalf("early settle status=%d body=%v", status, body)
	}
	// 此时还没有成交结果：404。
	status, body = doJSON(t, "GET", srv.URL+"/v1/auctions/1/settlement", nil)
	if status != http.StatusNotFound || body["code"] != "not_found" {
		t.Fatalf("early settlement get status=%d body=%v", status, body)
	}

	clk.Set(time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC))
	// 迟到报价：409 deadline_passed。
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/bids", map[string]any{
		"external_id": "late", "bidder": "L", "qty": 1, "price": "999",
	})
	if status != http.StatusConflict || body["code"] != "deadline_passed" {
		t.Fatalf("late bid status=%d body=%v", status, body)
	}

	// 首次清算 201；b2 已撤回，故只有 b1 成交 4。
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/settle", nil)
	if status != http.StatusCreated || body["outcome"] != "settled" {
		t.Fatalf("settle status=%d body=%v", status, body)
	}
	data := body["data"].(map[string]any)
	if data["sold_qty"].(float64) != 4 || data["clearing_price"] != "10" || data["total_proceeds"] != "40" {
		t.Fatalf("settlement data=%v", data)
	}
	fills := data["fills"].([]any)
	if len(fills) != 1 || fills[0].(map[string]any)["external_id"] != "b1" {
		t.Fatalf("fills=%v", fills)
	}
	// 重复清算：200 replayed。
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/settle", nil)
	if status != http.StatusOK || body["outcome"] != "replayed" {
		t.Fatalf("settle replay status=%d body=%v", status, body)
	}
	// GET 成交结果。
	status, body = doJSON(t, "GET", srv.URL+"/v1/auctions/1/settlement", nil)
	if status != http.StatusOK || body["sold_qty"].(float64) != 4 {
		t.Fatalf("get settlement status=%d body=%v", status, body)
	}
	// 清算后撤回：409 already_settled。
	status, body = doJSON(t, "POST", srv.URL+"/v1/auctions/1/bids/b1/withdraw", nil)
	if status != http.StatusConflict || body["code"] != "already_settled" {
		t.Fatalf("post-settle withdraw status=%d body=%v", status, body)
	}
}

func TestHTTPMalformedBody(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/auctions", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body status=%d", resp.StatusCode)
	}
}
