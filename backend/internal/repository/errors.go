package repository

import "errors"

// 仓储层哨兵错误。
var (
	ErrNotFound         = errors.New("repository: record not found")
	ErrQuotaExceeded    = errors.New("repository: budget quota exceeded")
	ErrConcurrentUpdate = errors.New("repository: concurrent update conflict")
)
