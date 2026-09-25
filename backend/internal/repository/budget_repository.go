package repository

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/renovation/renovation-budget-api/internal/model"
)

// BudgetListFilter 预算表列表过滤条件。
type BudgetListFilter struct {
	ProjectID string
	Status    string
	Page      int
	PageSize  int
}

// BudgetAmountPatch 预算表金额变动量，正数表示增加。
type BudgetAmountPatch struct {
	SpentDelta  float64
	FrozenDelta float64
}

// BudgetRepository 预算表数据访问接口。
type BudgetRepository interface {
	Create(ctx context.Context, sheet *model.BudgetSheet) error
	FindByID(ctx context.Context, id uint) (*model.BudgetSheet, error)
	List(ctx context.Context, filter BudgetListFilter) ([]model.BudgetSheet, int64, error)
	Update(ctx context.Context, sheet *model.BudgetSheet) error
	// UpdateBasics 仅更新预算表基础信息并用库内已支出/占用重算可用余额。
	UpdateBasics(ctx context.Context, sheet *model.BudgetSheet) error
	Delete(ctx context.Context, id uint) error
	// AdjustAmounts 原子调整整表已支出/占用金额并重算可用余额。
	AdjustAmounts(ctx context.Context, id uint, patch BudgetAmountPatch) error
}

type budgetRepository struct {
	db *gorm.DB
}

// NewBudgetRepository 构造预算表仓储。
func NewBudgetRepository(db *gorm.DB) BudgetRepository {
	return &budgetRepository{db: db}
}

func (r *budgetRepository) Create(ctx context.Context, sheet *model.BudgetSheet) error {
	if err := txFromContext(r.db, ctx).Create(sheet).Error; err != nil {
		return fmt.Errorf("create budget sheet: %w", err)
	}
	return nil
}

func (r *budgetRepository) FindByID(ctx context.Context, id uint) (*model.BudgetSheet, error) {
	var sheet model.BudgetSheet
	if err := txFromContext(r.db, ctx).Preload("Items").First(&sheet, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find budget sheet %d: %w", id, err)
	}
	return &sheet, nil
}

func (r *budgetRepository) List(ctx context.Context, filter BudgetListFilter) ([]model.BudgetSheet, int64, error) {
	page, pageSize := normalizePage(filter.Page, filter.PageSize)
	q := txFromContext(r.db, ctx).Model(&model.BudgetSheet{})
	if filter.ProjectID != "" {
		q = q.Where("project_id = ?", filter.ProjectID)
	}
	if filter.Status != "" {
		q = q.Where("status = ?", filter.Status)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count budget sheets: %w", err)
	}
	var sheets []model.BudgetSheet
	if err := q.Order("id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&sheets).Error; err != nil {
		return nil, 0, fmt.Errorf("list budget sheets: %w", err)
	}
	return sheets, total, nil
}

func (r *budgetRepository) Update(ctx context.Context, sheet *model.BudgetSheet) error {
	if err := txFromContext(r.db, ctx).Save(sheet).Error; err != nil {
		return fmt.Errorf("update budget sheet %d: %w", sheet.ID, err)
	}
	return nil
}

func (r *budgetRepository) UpdateBasics(ctx context.Context, sheet *model.BudgetSheet) error {
	result := txFromContext(r.db, ctx).
		Model(&model.BudgetSheet{}).
		Where("id = ?", sheet.ID).
		Updates(map[string]any{
			"project_id":       sheet.ProjectID,
			"name":             sheet.Name,
			"total_amount":     sheet.TotalAmount,
			"status":           sheet.Status,
			"available_amount": gorm.Expr("? - spent_amount - frozen_amount", sheet.TotalAmount),
			"version":          sheet.Version,
		})
	if result.Error != nil {
		return fmt.Errorf("update budget sheet basics %d: %w", sheet.ID, result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *budgetRepository) Delete(ctx context.Context, id uint) error {
	if err := txFromContext(r.db, ctx).Delete(&model.BudgetSheet{}, id).Error; err != nil {
		return fmt.Errorf("delete budget sheet %d: %w", id, err)
	}
	return nil
}

// AdjustAmounts 用条件 UPDATE 原子调整整表金额，避免并发审批下的丢失更新；
// 调整后不得出现负的已支出/占用（防御性约束）。
func (r *budgetRepository) AdjustAmounts(ctx context.Context, id uint, patch BudgetAmountPatch) error {
	result := txFromContext(r.db, ctx).
		Model(&model.BudgetSheet{}).
		Where("id = ?", id).
		Where("spent_amount + ? >= 0 AND frozen_amount + ? >= 0", patch.SpentDelta, patch.FrozenDelta).
		Updates(map[string]any{
			"spent_amount":     gorm.Expr("spent_amount + ?", patch.SpentDelta),
			"frozen_amount":    gorm.Expr("frozen_amount + ?", patch.FrozenDelta),
			"available_amount": gorm.Expr("total_amount - (spent_amount + ?) - (frozen_amount + ?)", patch.SpentDelta, patch.FrozenDelta),
		})
	if result.Error != nil {
		return fmt.Errorf("adjust amounts for budget sheet %d: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func normalizePage(page, pageSize int) (int, int) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}
