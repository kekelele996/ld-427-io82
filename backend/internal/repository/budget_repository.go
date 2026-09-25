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

// BudgetRepository 预算表数据访问接口。
type BudgetRepository interface {
	Create(ctx context.Context, sheet *model.BudgetSheet) error
	FindByID(ctx context.Context, id uint) (*model.BudgetSheet, error)
	List(ctx context.Context, filter BudgetListFilter) ([]model.BudgetSheet, int64, error)
	Update(ctx context.Context, sheet *model.BudgetSheet) error
	Delete(ctx context.Context, id uint) error
	// TryFreeze 在整表余额充足时原子地增加冻结金额，余额不足返回 ErrQuotaExceeded。
	TryFreeze(ctx context.Context, sheetID uint, amount float64) error
	// ReleaseFreeze 原子地释放冻结金额，冻结不足返回 ErrConcurrentUpdate。
	ReleaseFreeze(ctx context.Context, sheetID uint, amount float64) error
	// ConvertFreezeToSpent 原子地把冻结金额转为已支出金额，冻结不足返回 ErrConcurrentUpdate。
	ConvertFreezeToSpent(ctx context.Context, sheetID uint, amount float64) error
}

type budgetRepository struct {
	db *gorm.DB
}

// NewBudgetRepository 构造预算表仓储。
func NewBudgetRepository(db *gorm.DB) BudgetRepository {
	return &budgetRepository{db: db}
}

func (r *budgetRepository) Create(ctx context.Context, sheet *model.BudgetSheet) error {
	if err := withTx(ctx, r.db).Create(sheet).Error; err != nil {
		return fmt.Errorf("create budget sheet: %w", err)
	}
	return nil
}

func (r *budgetRepository) FindByID(ctx context.Context, id uint) (*model.BudgetSheet, error) {
	var sheet model.BudgetSheet
	if err := withTx(ctx, r.db).Preload("Items").First(&sheet, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find budget sheet %d: %w", id, err)
	}
	return &sheet, nil
}

func (r *budgetRepository) List(ctx context.Context, filter BudgetListFilter) ([]model.BudgetSheet, int64, error) {
	page, pageSize := normalizePage(filter.Page, filter.PageSize)
	q := withTx(ctx, r.db).Model(&model.BudgetSheet{})
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
	if err := withTx(ctx, r.db).Save(sheet).Error; err != nil {
		return fmt.Errorf("update budget sheet %d: %w", sheet.ID, err)
	}
	return nil
}

func (r *budgetRepository) Delete(ctx context.Context, id uint) error {
	if err := withTx(ctx, r.db).Delete(&model.BudgetSheet{}, id).Error; err != nil {
		return fmt.Errorf("delete budget sheet %d: %w", id, err)
	}
	return nil
}

// TryFreeze 以条件更新原子地冻结整表余额：
// total - spent - frozen >= amount 才放行，与分项占用在同一事务内生效。
func (r *budgetRepository) TryFreeze(ctx context.Context, sheetID uint, amount float64) error {
	res := withTx(ctx, r.db).Model(&model.BudgetSheet{}).
		Where("id = ? AND total_amount - spent_amount - frozen_amount >= ?", sheetID, amount).
		Updates(map[string]any{
			"frozen_amount":    gorm.Expr("frozen_amount + ?", amount),
			"available_amount": gorm.Expr("total_amount - spent_amount - (frozen_amount + ?)", amount),
		})
	if res.Error != nil {
		return fmt.Errorf("freeze budget sheet %d: %w", sheetID, res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrQuotaExceeded
	}
	return nil
}

// ReleaseFreeze 原子地释放整表冻结金额。
func (r *budgetRepository) ReleaseFreeze(ctx context.Context, sheetID uint, amount float64) error {
	res := withTx(ctx, r.db).Model(&model.BudgetSheet{}).
		Where("id = ? AND frozen_amount >= ?", sheetID, amount).
		Updates(map[string]any{
			"frozen_amount":    gorm.Expr("frozen_amount - ?", amount),
			"available_amount": gorm.Expr("total_amount - spent_amount - (frozen_amount - ?)", amount),
		})
	if res.Error != nil {
		return fmt.Errorf("release budget sheet %d: %w", sheetID, res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrConcurrentUpdate
	}
	return nil
}

// ConvertFreezeToSpent 原子地把整表冻结金额转为已支出金额。
func (r *budgetRepository) ConvertFreezeToSpent(ctx context.Context, sheetID uint, amount float64) error {
	res := withTx(ctx, r.db).Model(&model.BudgetSheet{}).
		Where("id = ? AND frozen_amount >= ?", sheetID, amount).
		Updates(map[string]any{
			"frozen_amount":    gorm.Expr("frozen_amount - ?", amount),
			"spent_amount":     gorm.Expr("spent_amount + ?", amount),
			"available_amount": gorm.Expr("total_amount - (spent_amount + ?) - (frozen_amount - ?)", amount, amount),
		})
	if res.Error != nil {
		return fmt.Errorf("convert budget sheet %d freeze to spent: %w", sheetID, res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrConcurrentUpdate
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
