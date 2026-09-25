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
	// UpdateBasics 仅更新分项基础信息并用库内已支出/占用重算额度；
	// 新预算金额不得小于已支出 + 占用，否则返回 ErrBudgetConflict。
	UpdateBasics(ctx context.Context, item *model.BudgetItem) error
	Delete(ctx context.Context, id uint) error
	// ReserveFrozen 原子增加分项占用金额；剩余可用额度不足时返回 ErrBudgetConflict。
	ReserveFrozen(ctx context.Context, itemID uint, amount float64) error
	// ConfirmFrozen 原子地把分项占用金额转为已支出金额。
	ConfirmFrozen(ctx context.Context, itemID uint, amount float64) error
	// ReleaseFrozen 原子释放分项占用金额；占用额不足时返回 ErrInvalidState。
	ReleaseFrozen(ctx context.Context, itemID uint, amount float64) error
}

type itemRepository struct {
	db *gorm.DB
}

// NewItemRepository 构造预算项仓储。
func NewItemRepository(db *gorm.DB) ItemRepository {
	return &itemRepository{db: db}
}

func (r *itemRepository) Create(ctx context.Context, item *model.BudgetItem) error {
	if err := txFromContext(r.db, ctx).Create(item).Error; err != nil {
		return fmt.Errorf("create budget item: %w", err)
	}
	return nil
}

func (r *itemRepository) FindByID(ctx context.Context, id uint) (*model.BudgetItem, error) {
	var item model.BudgetItem
	if err := txFromContext(r.db, ctx).First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find budget item %d: %w", id, err)
	}
	return &item, nil
}

func (r *itemRepository) ListByBudgetID(ctx context.Context, budgetSheetID uint) ([]model.BudgetItem, error) {
	var items []model.BudgetItem
	if err := txFromContext(r.db, ctx).Where("budget_sheet_id = ?", budgetSheetID).Order("sort_order ASC, id ASC").Find(&items).Error; err != nil {
		return nil, fmt.Errorf("list budget items for sheet %d: %w", budgetSheetID, err)
	}
	return items, nil
}

func (r *itemRepository) Update(ctx context.Context, item *model.BudgetItem) error {
	if err := txFromContext(r.db, ctx).Save(item).Error; err != nil {
		return fmt.Errorf("update budget item %d: %w", item.ID, err)
	}
	return nil
}

func (r *itemRepository) UpdateBasics(ctx context.Context, item *model.BudgetItem) error {
	result := txFromContext(r.db, ctx).
		Model(&model.BudgetItem{}).
		Where("id = ? AND budget_amount >= spent_amount + frozen_amount", item.BudgetAmount).
		Updates(map[string]any{
			"category":         item.Category,
			"sub_category":     item.SubCategory,
			"budget_amount":    item.BudgetAmount,
			"variance_amount":  gorm.Expr("spent_amount - ?", item.BudgetAmount),
			"available_amount": gorm.Expr("? - spent_amount - frozen_amount", item.BudgetAmount),
			"sort_order":       item.SortOrder,
			"remark":           item.Remark,
		})
	if result.Error != nil {
		return fmt.Errorf("update budget item basics %d: %w", item.ID, result.Error)
	}
	if result.RowsAffected == 0 {
		var count int64
		if err := txFromContext(r.db, ctx).Model(&model.BudgetItem{}).Where("id = ?", item.ID).Count(&count).Error; err != nil {
			return fmt.Errorf("check budget item %d after update: %w", item.ID, err)
		}
		if count == 0 {
			return ErrNotFound
		}
		return ErrBudgetConflict
	}
	return nil
}

func (r *itemRepository) Delete(ctx context.Context, id uint) error {
	if err := txFromContext(r.db, ctx).Delete(&model.BudgetItem{}, id).Error; err != nil {
		return fmt.Errorf("delete budget item %d: %w", id, err)
	}
	return nil
}

// ReserveFrozen 在单条 UPDATE 中完成「余额校验 + 占用」，行锁保证并发提交串行化：
// frozen_amount + amount 不得超过 budget_amount - spent_amount。
func (r *itemRepository) ReserveFrozen(ctx context.Context, itemID uint, amount float64) error {
	result := txFromContext(r.db, ctx).
		Model(&model.BudgetItem{}).
		Where("id = ? AND budget_amount - spent_amount - frozen_amount >= ?", itemID, amount).
		Updates(map[string]any{
			"frozen_amount":    gorm.Expr("frozen_amount + ?", amount),
			"available_amount": gorm.Expr("budget_amount - spent_amount - (frozen_amount + ?)", amount),
		})
	if result.Error != nil {
		return fmt.Errorf("reserve frozen for budget item %d: %w", itemID, result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrBudgetConflict
	}
	return nil
}

// ConfirmFrozen 把审批通过的占用额结转为已支出额，并同步差异与可用额度。
func (r *itemRepository) ConfirmFrozen(ctx context.Context, itemID uint, amount float64) error {
	result := txFromContext(r.db, ctx).
		Model(&model.BudgetItem{}).
		Where("id = ? AND frozen_amount >= ?", itemID, amount).
		Updates(map[string]any{
			"frozen_amount":    gorm.Expr("frozen_amount - ?", amount),
			"spent_amount":     gorm.Expr("spent_amount + ?", amount),
			"available_amount": gorm.Expr("budget_amount - (spent_amount + ?) - (frozen_amount - ?)", amount, amount),
			"variance_amount":  gorm.Expr("(spent_amount + ?) - budget_amount", amount),
		})
	if result.Error != nil {
		return fmt.Errorf("confirm frozen for budget item %d: %w", itemID, result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrInvalidState
	}
	return nil
}

// ReleaseFrozen 释放被驳回的占用额，可用额度恢复。
func (r *itemRepository) ReleaseFrozen(ctx context.Context, itemID uint, amount float64) error {
	result := txFromContext(r.db, ctx).
		Model(&model.BudgetItem{}).
		Where("id = ? AND frozen_amount >= ?", itemID, amount).
		Updates(map[string]any{
			"frozen_amount":    gorm.Expr("frozen_amount - ?", amount),
			"available_amount": gorm.Expr("budget_amount - spent_amount - (frozen_amount - ?)", amount),
		})
	if result.Error != nil {
		return fmt.Errorf("release frozen for budget item %d: %w", itemID, result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrInvalidState
	}
	return nil
}
