package service

import "errors"

// 服务层哨兵错误。
var (
	ErrNotFound            = errors.New("service: not found")
	ErrInvalidLogin        = errors.New("service: invalid username or password")
	ErrInvalidState        = errors.New("service: invalid state transition")
	ErrInsufficientBalance = errors.New("service: insufficient available balance")
	ErrForbiddenTransition = errors.New("service: forbidden transition")
	// ErrItemBudgetExceeded 提交支出超过分项预算（已支出 + 审批中占用 + 本笔金额）。
	ErrItemBudgetExceeded = errors.New("service: budget item limit exceeded")
)
