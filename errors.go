package auctionclearing

import "errors"

var (
	ErrNotFound            = errors.New("auctionclearing: not found")
	ErrInvalidState        = errors.New("auctionclearing: invalid state")
	ErrValidation          = errors.New("auctionclearing: validation failed")
	ErrVersionConflict     = errors.New("auctionclearing: settlement version conflict")
	ErrBatchClosed         = errors.New("auctionclearing: delivery batch already closed")
	ErrBatchNotClosable    = errors.New("auctionclearing: delivery batch not closable")
	ErrIdempotencyConflict = errors.New("auctionclearing: idempotency conflict")
)
