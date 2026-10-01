package auctionclearing

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Server 把应用服务暴露为 JSON HTTP 接口，
// 并把领域错误类别映射为合适的 HTTP 状态码：
//
//	invalid_argument -> 400, not_found -> 404,
//	conflict        -> 409, invalid_state -> 422（含幂等冲突以外的状态错误）。
type Server struct {
	svc *Service
	mux *http.ServeMux
}

// NewServer 构造 HTTP 服务。
func NewServer(svc *Service) *Server {
	s := &Server{svc: svc, mux: http.NewServeMux()}
	s.routes()
	return s
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /api/v1/auctions", s.createAuction)
	s.mux.HandleFunc("GET /api/v1/auctions/{id}", s.getAuction)
	s.mux.HandleFunc("POST /api/v1/auctions/{id}/bids", s.placeBid)
	s.mux.HandleFunc("GET /api/v1/auctions/{id}/bids/{ref}", s.getBid)
	s.mux.HandleFunc("DELETE /api/v1/auctions/{id}/bids/{ref}", s.withdrawBid)
	s.mux.HandleFunc("POST /api/v1/auctions/{id}/clear", s.clearAuction)
	s.mux.HandleFunc("GET /api/v1/auctions/{id}/result", s.getResult)
	s.mux.HandleFunc("POST /api/v1/auctions/{id}/settle", s.settleAuction)
	s.mux.HandleFunc("GET /api/v1/auctions/{id}/settlement", s.getSettlement)
	s.mux.HandleFunc("POST /api/v1/auctions/{id}/fills/{ref}/confirm", s.confirmFill)
	s.mux.HandleFunc("POST /api/v1/auctions/{id}/fills/{ref}/freeze", s.freezeFunds)
	s.mux.HandleFunc("POST /api/v1/auctions/{id}/fills/{ref}/deliver", s.markDelivered)
	s.mux.HandleFunc("POST /api/v1/auctions/{id}/corrections", s.submitCorrection)
	s.mux.HandleFunc("GET /api/v1/auctions/{id}/corrections", s.listCorrections)
	s.mux.HandleFunc("GET /api/v1/auctions/{id}/corrections/{cid}", s.getCorrection)
	s.mux.HandleFunc("GET /api/v1/auctions/{id}/corrections/{cid}/trace", s.getCorrectionTrace)
	s.mux.HandleFunc("POST /api/v1/auctions/{id}/corrections/{cid}/approve", s.approveCorrection)
	s.mux.HandleFunc("POST /api/v1/auctions/{id}/corrections/{cid}/retry-compensation", s.retryCompensation)
}

// createAuctionRequest 创建拍卖请求体；金额为十进制字符串，时间为 RFC3339。
type createAuctionRequest struct {
	AvailableQty int64  `json:"available_qty"`
	Deadline     string `json:"deadline"`
	ReservePrice string `json:"reserve_price"`
}

// placeBidRequest 报价请求体。
type placeBidRequest struct {
	ExternalRef string `json:"external_ref"`
	Bidder      string `json:"bidder"`
	Quantity    int64  `json:"quantity"`
	UnitPrice   string `json:"unit_price"`
}

// deliverRequest 交割请求；quantity 为本次增量交割数量。
type deliverRequest struct {
	Quantity int64 `json:"quantity"`
}

// submitCorrectionRequest 更正申请请求体。
type submitCorrectionRequest struct {
	CorrectionID string `json:"correction_id"`
	Reason       string `json:"reason"`
	Basis        string `json:"basis"`
	Handler      string `json:"handler"`
	Items        []struct {
		ExternalRef     string `json:"external_ref"`
		TargetUnitPrice string `json:"target_unit_price"`
	} `json:"items"`
}

// approveCorrectionRequest 批准更正请求体（handler/basis 可省略，沿用申请值）。
type approveCorrectionRequest struct {
	Handler string `json:"handler"`
	Basis   string `json:"basis"`
}

type errorResponse struct {
	Error struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *Server) createAuction(w http.ResponseWriter, r *http.Request) {
	var req createAuctionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	reserve, err := ParseMoney(req.ReservePrice)
	if err != nil {
		writeDomainError(w, NewError(KindInvalidArgument, "reserve_price: "+err.Error()))
		return
	}
	deadline, err := time.Parse(time.RFC3339, req.Deadline)
	if err != nil {
		writeDomainError(w, NewError(KindInvalidArgument, "deadline must be RFC3339 time"))
		return
	}
	a, err := s.svc.CreateAuction(r.Context(), CreateAuctionParams{
		AvailableQty: req.AvailableQty,
		Deadline:     deadline,
		ReservePrice: reserve,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) getAuction(w http.ResponseWriter, r *http.Request) {
	a, err := s.svc.GetAuction(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) placeBid(w http.ResponseWriter, r *http.Request) {
	var req placeBidRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	price, err := ParseMoney(req.UnitPrice)
	if err != nil {
		writeDomainError(w, NewError(KindInvalidArgument, "unit_price: "+err.Error()))
		return
	}
	out, err := s.svc.PlaceBid(r.Context(), r.PathValue("id"), PlaceBidParams{
		ExternalRef: req.ExternalRef,
		Bidder:      req.Bidder,
		Quantity:    req.Quantity,
		UnitPrice:   price,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	// 幂等重放返回 200，新建报价返回 201，语义清晰可辨。
	status := http.StatusCreated
	if out.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"bid": out.Bid, "replayed": out.Replayed})
}

func (s *Server) getBid(w http.ResponseWriter, r *http.Request) {
	b, err := s.svc.GetBid(r.Context(), r.PathValue("id"), r.PathValue("ref"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) withdrawBid(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.WithdrawBid(r.Context(), r.PathValue("id"), r.PathValue("ref"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bid": out.Bid, "replayed": out.Replayed})
}

func (s *Server) clearAuction(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.ClearAuction(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	// 首次清算 201；重复请求读到原结果返回 200。
	status := http.StatusCreated
	if out.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"result": out.Result, "replayed": out.Replayed})
}

func (s *Server) getResult(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.GetResult(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) settleAuction(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.SettleAuction(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	status := http.StatusCreated
	if out.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"settlement": out.Settlement, "replayed": out.Replayed})
}

func (s *Server) getSettlement(w http.ResponseWriter, r *http.Request) {
	st, err := s.svc.GetSettlement(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) confirmFill(w http.ResponseWriter, r *http.Request) {
	e, err := s.svc.ConfirmFill(r.Context(), r.PathValue("id"), r.PathValue("ref"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) freezeFunds(w http.ResponseWriter, r *http.Request) {
	e, err := s.svc.FreezeFunds(r.Context(), r.PathValue("id"), r.PathValue("ref"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) markDelivered(w http.ResponseWriter, r *http.Request) {
	var req deliverRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	e, err := s.svc.MarkDelivered(r.Context(), r.PathValue("id"), r.PathValue("ref"), req.Quantity)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) submitCorrection(w http.ResponseWriter, r *http.Request) {
	var req submitCorrectionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	p := SubmitCorrectionParams{
		CorrectionID: req.CorrectionID,
		Reason:       req.Reason,
		Basis:        req.Basis,
		Handler:      req.Handler,
		Items:        make([]CorrectionItem, 0, len(req.Items)),
	}
	for _, it := range req.Items {
		price, err := ParseMoney(it.TargetUnitPrice)
		if err != nil {
			writeDomainError(w, NewError(KindInvalidArgument, "target_unit_price: "+err.Error()))
			return
		}
		p.Items = append(p.Items, CorrectionItem{ExternalRef: it.ExternalRef, TargetUnitPrice: price})
	}
	out, err := s.svc.SubmitCorrection(r.Context(), r.PathValue("id"), p)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	status := http.StatusCreated
	if out.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"application": out.Application, "replayed": out.Replayed})
}

func (s *Server) listCorrections(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.ListCorrections(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"corrections": list})
}

func (s *Server) getCorrection(w http.ResponseWriter, r *http.Request) {
	app, err := s.svc.GetCorrection(r.Context(), r.PathValue("id"), r.PathValue("cid"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (s *Server) getCorrectionTrace(w http.ResponseWriter, r *http.Request) {
	trace, err := s.svc.GetCorrectionTrace(r.Context(), r.PathValue("id"), r.PathValue("cid"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, trace)
}

func (s *Server) approveCorrection(w http.ResponseWriter, r *http.Request) {
	var req approveCorrectionRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	app, replayed, err := s.svc.ApproveCorrection(r.Context(), r.PathValue("id"), r.PathValue("cid"),
		ApproveCorrectionParams{Handler: req.Handler, Basis: req.Basis})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"application": app, "replayed": replayed})
}

func (s *Server) retryCompensation(w http.ResponseWriter, r *http.Request) {
	app, err := s.svc.RetryCompensation(r.Context(), r.PathValue("id"), r.PathValue("cid"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeDomainError(w, NewError(KindInvalidArgument, "invalid JSON request body: "+err.Error()))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func writeDomainError(w http.ResponseWriter, err error) {
	var resp errorResponse
	kind := ErrorKindOf(err)
	if kind == "" {
		kind = KindInvalidArgument
	}
	resp.Error.Kind = string(kind)
	resp.Error.Message = strings.TrimPrefix(err.Error(), string(kind)+": ")
	writeJSON(w, statusForKind(kind), resp)
}

func statusForKind(kind ErrorKind) int {
	switch kind {
	case KindNotFound:
		return http.StatusNotFound
	case KindConflict:
		return http.StatusConflict
	case KindInvalidState:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusBadRequest
	}
}
