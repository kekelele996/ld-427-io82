package repository

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/renovation/renovation-budget-api/internal/model"
)

// ItemRepository 预算项数据访问接口。
type ItemRepository interface {
	Create(ctx context.Context, item *model.BudgetItem) error
	FindByID(ctx context.Context, id uint) (*model.BudgetItem, error)
	ListByBudgetID(ctx context.Context, budgetSheetID uint) ([]model.BudgetItem, error)
	Update(ctx context.Context, item *model.BudgetItem) error
	Delete(ctx context.Context, id uint) error
	// TryFreeze 在分项额度充足时原子地增加占用金额，额度不足返回 ErrQuotaExceeded。
	TryFreeze(ctx context.Context, itemID uint, amount float64) error
	// ReleaseFreeze 原子地释放占用金额，占用不足返回 ErrConcurrentUpdate。
	ReleaseFreeze(ctx context.Context, itemID uint, amount float64) error
	// ConvertFreezeToSpent 原子地把占用金额转为已支出金额，占用不足返回 ErrConcurrentUpdate。
	ConvertFreezeToSpent(ctx context.Context, itemID uint, amount float64) error
}

type itemRepository struct {
	db *gorm.DB
}

// NewItemRepository 构造预算项仓储。
func NewItemRepository(db *gorm.DB) ItemRepository {
	return &itemRepository{db: db}
}

func (r *itemRepository) Create(ctx context.Context, item *model.BudgetItem) error {
	if err := withTx(ctx, r.db).Create(item).Error; err != nil {
		return fmt.Errorf("create budget item: %w", err)
	}
	return nil
}

func (r *itemRepository) FindByID(ctx context.Context, id uint) (*model.BudgetItem, error) {
	var item model.BudgetItem
	if err := withTx(ctx, r.db).First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find budget item %d: %w", id, err)
	}
	return &item, nil
}

func (r *itemRepository) ListByBudgetID(ctx context.Context, budgetSheetID uint) ([]model.BudgetItem, error) {
	var items []model.BudgetItem
	if err := withTx(ctx, r.db).Where("budget_sheet_id = ?", budgetSheetID).Order("sort_order ASC, id ASC").Find(&items).Error; err != nil {
		return nil, fmt.Errorf("list budget items for sheet %d: %w", budgetSheetID, err)
	}
	return items, nil
}

func (r *itemRepository) Update(ctx context.Context, item *model.BudgetItem) error {
	if err := withTx(ctx, r.db).Save(item).Error; err != nil {
		return fmt.Errorf("update budget item %d: %w", item.ID, err)
	}
	return nil
}

func (r *itemRepository) Delete(ctx context.Context, id uint) error {
	if err := withTx(ctx, r.db).Delete(&model.BudgetItem{}, id).Error; err != nil {
		return fmt.Errorf("delete budget item %d: %w", id, err)
	}
	return nil
}

// TryFreeze 以条件更新原子地占用分项额度：
// spent + frozen + amount <= budget_amount 才放行，PostgreSQL 行锁串行化同一分项的并发提交。
func (r *itemRepository) TryFreeze(ctx context.Context, itemID uint, amount float64) error {
	res := withTx(ctx, r.db).Model(&model.BudgetItem{}).
		Where("id = ? AND budget_amount - spent_amount - frozen_amount >= ?", itemID, amount).
		Updates(map[string]any{
			"frozen_amount":    gorm.Expr("frozen_amount + ?", amount),
			"available_amount": gorm.Expr("budget_amount - spent_amount - (frozen_amount + ?)", amount),
		})
	if res.Error != nil {
		return fmt.Errorf("freeze budget item %d: %w", itemID, res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrQuotaExceeded
	}
	return nil
}

// ReleaseFreeze 原子地释放分项占用金额。
func (r *itemRepository) ReleaseFreeze(ctx context.Context, itemID uint, amount float64) error {
	res := withTx(ctx, r.db).Model(&model.BudgetItem{}).
		Where("id = ? AND frozen_amount >= ?", itemID, amount).
		Updates(map[string]any{
			"frozen_amount":    gorm.Expr("frozen_amount - ?", amount),
			"available_amount": gorm.Expr("budget_amount - spent_amount - (frozen_amount - ?)", amount),
		})
	if res.Error != nil {
		return fmt.Errorf("release budget item %d: %w", itemID, res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrConcurrentUpdate
	}
	return nil
}

// ConvertFreezeToSpent 原子地把分项占用金额转为已支出金额，并重算差异金额。
func (r *itemRepository) ConvertFreezeToSpent(ctx context.Context, itemID uint, amount float64) error {
	res := withTx(ctx, r.db).Model(&model.BudgetItem{}).
		Where("id = ? AND frozen_amount >= ?", itemID, amount).
		Updates(map[string]any{
			"frozen_amount":    gorm.Expr("frozen_amount - ?", amount),
			"spent_amount":     gorm.Expr("spent_amount + ?", amount),
			"variance_amount":  gorm.Expr("(spent_amount + ?) - budget_amount", amount),
			"available_amount": gorm.Expr("budget_amount - (spent_amount + ?) - (frozen_amount - ?)", amount, amount),
		})
	if res.Error != nil {
		return fmt.Errorf("convert budget item %d freeze to spent: %w", itemID, res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrConcurrentUpdate
	}
	return nil
}
