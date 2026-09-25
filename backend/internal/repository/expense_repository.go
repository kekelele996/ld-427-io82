package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/renovation/renovation-budget-api/internal/constants"
	"github.com/renovation/renovation-budget-api/internal/model"
)

// ExpenseListFilter 支出记录列表过滤条件。
type ExpenseListFilter struct {
	Status       string
	BudgetItemID uint
	SupplierID   uint
	Page         int
	PageSize     int
}

// ExpenseStatusPatch 支出记录状态流转的更新字段。
type ExpenseStatusPatch struct {
	Status          constants.ExpenseStatus
	ApprovedByID    *uint
	ApprovalComment string
	PaymentDate     *time.Time
}

// ExpenseRepository 支出记录数据访问接口。
type ExpenseRepository interface {
	Create(ctx context.Context, record *model.ExpenseRecord) error
	FindByID(ctx context.Context, id uint) (*model.ExpenseRecord, error)
	List(ctx context.Context, filter ExpenseListFilter) ([]model.ExpenseRecord, int64, error)
	Update(ctx context.Context, record *model.ExpenseRecord) error
	// TransitionStatus 仅当记录当前状态为 wantStatus 时原子更新；
	// 记录不存在返回 ErrNotFound，状态不匹配返回 ErrInvalidState。
	TransitionStatus(ctx context.Context, id uint, wantStatus constants.ExpenseStatus, patch ExpenseStatusPatch) error
}

type expenseRepository struct {
	db *gorm.DB
}

// NewExpenseRepository 构造支出记录仓储。
func NewExpenseRepository(db *gorm.DB) ExpenseRepository {
	return &expenseRepository{db: db}
}

func (r *expenseRepository) Create(ctx context.Context, record *model.ExpenseRecord) error {
	if err := txFromContext(r.db, ctx).Create(record).Error; err != nil {
		return fmt.Errorf("create expense record: %w", err)
	}
	return nil
}

func (r *expenseRepository) FindByID(ctx context.Context, id uint) (*model.ExpenseRecord, error) {
	var record model.ExpenseRecord
	if err := txFromContext(r.db, ctx).First(&record, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find expense record %d: %w", id, err)
	}
	return &record, nil
}

func (r *expenseRepository) List(ctx context.Context, filter ExpenseListFilter) ([]model.ExpenseRecord, int64, error) {
	page, pageSize := normalizePage(filter.Page, filter.PageSize)
	q := txFromContext(r.db, ctx).Model(&model.ExpenseRecord{})
	if filter.Status != "" {
		q = q.Where("status = ?", filter.Status)
	}
	if filter.BudgetItemID != 0 {
		q = q.Where("budget_item_id = ?", filter.BudgetItemID)
	}
	if filter.SupplierID != 0 {
		q = q.Where("supplier_id = ?", filter.SupplierID)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count expense records: %w", err)
	}
	var records []model.ExpenseRecord
	if err := q.Order("id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&records).Error; err != nil {
		return nil, 0, fmt.Errorf("list expense records: %w", err)
	}
	return records, total, nil
}

func (r *expenseRepository) Update(ctx context.Context, record *model.ExpenseRecord) error {
	if err := txFromContext(r.db, ctx).Save(record).Error; err != nil {
		return fmt.Errorf("update expense record %d: %w", record.ID, err)
	}
	return nil
}

func (r *expenseRepository) TransitionStatus(ctx context.Context, id uint, wantStatus constants.ExpenseStatus, patch ExpenseStatusPatch) error {
	values := map[string]any{
		"status": patch.Status,
	}
	if patch.ApprovedByID != nil {
		values["approved_by_id"] = *patch.ApprovedByID
	}
	if patch.ApprovalComment != "" {
		values["approval_comment"] = patch.ApprovalComment
	}
	if patch.PaymentDate != nil {
		values["payment_date"] = *patch.PaymentDate
	}
	result := txFromContext(r.db, ctx).
		Model(&model.ExpenseRecord{}).
		Where("id = ? AND status = ?", id, wantStatus).
		Updates(values)
	if result.Error != nil {
		return fmt.Errorf("transition status for expense record %d: %w", id, result.Error)
	}
	if result.RowsAffected == 0 {
		// 区分不存在与状态已被并发请求改变。
		var count int64
		if err := txFromContext(r.db, ctx).Model(&model.ExpenseRecord{}).Where("id = ?", id).Count(&count).Error; err != nil {
			return fmt.Errorf("check expense record %d after failed transition: %w", id, err)
		}
		if count == 0 {
			return ErrNotFound
		}
		return ErrInvalidState
	}
	return nil
}
