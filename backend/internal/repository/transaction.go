package repository

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// txContextKey 在 context 中传递当前事务的键。
type txContextKey struct{}

// TransactionManager 管理跨仓储的数据库事务。
type TransactionManager interface {
	WithinTransaction(ctx context.Context, fn func(txCtx context.Context) error) error
}

// txFromContext 取出 context 中的事务，不存在时返回默认 db。
func txFromContext(db *gorm.DB, ctx context.Context) *gorm.DB {
	if tx, ok := ctx.Value(txContextKey{}).(*gorm.DB); ok && tx != nil {
		return tx.WithContext(ctx)
	}
	return db.WithContext(ctx)
}

type transactionManager struct {
	db *gorm.DB
}

// NewTransactionManager 构造事务管理器。
func NewTransactionManager(db *gorm.DB) TransactionManager {
	return &transactionManager{db: db}
}

func (m *transactionManager) WithinTransaction(ctx context.Context, fn func(txCtx context.Context) error) error {
	if tx, ok := ctx.Value(txContextKey{}).(*gorm.DB); ok && tx != nil {
		// 已在事务中时复用当前事务（支持嵌套调用）。
		return fn(ctx)
	}
	err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(context.WithValue(ctx, txContextKey{}, tx))
	})
	if err != nil {
		return fmt.Errorf("within transaction: %w", err)
	}
	return nil
}
