package auctionclearing

import "errors"

var (
	// ErrTradeNotFound 成交不存在。
	ErrTradeNotFound = errors.New("trade not found")
	// ErrBatchNotFound 交割批次不存在。
	ErrBatchNotFound = errors.New("delivery batch not found")
	// ErrTradeNotCleared 成交尚未清算完成,不能加入交割批次。
	ErrTradeNotCleared = errors.New("trade is not cleared")
	// ErrTradeAlreadyInBatch 成交已属于一个未完成的交割批次。
	ErrTradeAlreadyInBatch = errors.New("trade already belongs to an open batch")
	// ErrTradeAlreadyDelivered 成交已完成交割,不能再次加入新批次。
	ErrTradeAlreadyDelivered = errors.New("trade already fully delivered")
	// ErrVersionConflict 结算版本冲突:更正先生效,旧批次不得继续确认。
	ErrVersionConflict = errors.New("settlement version conflict")
	// ErrBatchClosed 批次已关闭,迟到确认返回明确冲突。
	ErrBatchClosed = errors.New("delivery batch closed")
	// ErrBatchNotClosable 批次内仍有未完成且未取消的成交,不能关闭。
	ErrBatchNotClosable = errors.New("batch has unfinished items")
	// ErrItemNotFound 批次内不存在该成交。
	ErrItemNotFound = errors.New("trade not in batch")
	// ErrItemFinished 该成交在批次内已完成或已取消。
	ErrItemFinished = errors.New("batch item already finished")
	// ErrOverDelivery 交割数量超过剩余数量。
	ErrOverDelivery = errors.New("delivered quantity exceeds remaining quantity")
	// ErrInvalidQuantity 数量非法。
	ErrInvalidQuantity = errors.New("invalid quantity")
	// ErrIdempotencyConflict 相同外部操作号但内容不同。
	ErrIdempotencyConflict = errors.New("idempotency key replayed with different payload")
)
