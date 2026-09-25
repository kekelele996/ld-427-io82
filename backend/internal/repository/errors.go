package repository

import "errors"

// 仓储层哨兵错误。
var (
	ErrNotFound       = errors.New("repository: record not found")
	ErrInvalidState   = errors.New("repository: invalid state transition")
	ErrBudgetConflict = errors.New("repository: budget item limit exceeded")
)
