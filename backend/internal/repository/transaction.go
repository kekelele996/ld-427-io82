package repository

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// txCtxKey 在 context 中传递事务句柄。
type txCtxKey struct{}

// withTx 将事务句柄注入 context；未开启事务时返回默认句柄。
func withTx(ctx context.Context, db *gorm.DB) *gorm.DB {
	if tx, ok := ctx.Value(txCtxKey{}).(*gorm.DB); ok && tx != nil {
		return tx.WithContext(ctx)
	}
	return db.WithContext(ctx)
}

// Transactor 在一个数据库事务内执行 fn；fn 通过传入的 context 复用同一事务。
type Transactor interface {
	WithinTransaction(ctx context.Context, fn func(txCtx context.Context) error) error
}

type transactor struct {
	db *gorm.DB
}

// NewTransactor 构造事务执行器。
func NewTransactor(db *gorm.DB) Transactor {
	return &transactor{db: db}
}

func (t *transactor) WithinTransaction(ctx context.Context, fn func(txCtx context.Context) error) (err error) {
	tx := t.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return fmt.Errorf("begin transaction: %w", tx.Error)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback().Error
			panic(p)
		}
	}()
	if err = fn(context.WithValue(ctx, txCtxKey{}, tx)); err != nil {
		if rbErr := tx.Rollback().Error; rbErr != nil {
			return fmt.Errorf("rollback transaction: %w (%v)", rbErr, err)
		}
		return err
	}
	if err = tx.Commit().Error; err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
