package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/renovation/renovation-budget-api/internal/constants"
	"github.com/renovation/renovation-budget-api/internal/dto"
	"github.com/renovation/renovation-budget-api/internal/model"
	"github.com/renovation/renovation-budget-api/internal/repository"
)

// ExpenseService 支出记录业务逻辑。
type ExpenseService struct {
	repo       repository.ExpenseRepository
	itemRepo   repository.ItemRepository
	budgetRepo repository.BudgetRepository
	tx         repository.Transactor
	audit      *AuditService
	rdb        *redis.Client
	logger     *slog.Logger
}

// NewExpenseService 构造支出记录服务。
func NewExpenseService(repo repository.ExpenseRepository, itemRepo repository.ItemRepository, budgetRepo repository.BudgetRepository, tx repository.Transactor, audit *AuditService, rdb *redis.Client, logger *slog.Logger) *ExpenseService {
	return &ExpenseService{repo: repo, itemRepo: itemRepo, budgetRepo: budgetRepo, tx: tx, audit: audit, rdb: rdb, logger: logger}
}

// Create 创建草稿支出记录。
func (s *ExpenseService) Create(ctx context.Context, actor model.Actor, req dto.CreateExpenseRequest) (*model.ExpenseRecord, error) {
	item, err := s.itemRepo.FindByID(ctx, req.BudgetItemID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get budget item %d: %w", req.BudgetItemID, err)
	}
	expenseDate, err := time.Parse("2006-01-02", req.ExpenseDate)
	if err != nil {
		return nil, fmt.Errorf("parse expense date: %w", err)
	}
	record := &model.ExpenseRecord{
		BudgetItemID:  item.ID,
		Amount:        req.Amount,
		ExpenseDate:   expenseDate,
		PaymentMethod: req.PaymentMethod,
		SupplierID:    req.SupplierID,
		InvoiceNo:     req.InvoiceNo,
		Description:   req.Description,
		AttachmentURL: req.AttachmentURL,
		Status:        constants.ExpenseStatusDraft,
		ApplicantID:   actor.UserID,
	}
	if err := s.repo.Create(ctx, record); err != nil {
		return nil, fmt.Errorf("create expense record: %w", err)
	}
	s.audit.Record(ctx, actor, "expense_create", "expense", record.ID, fmt.Sprintf("budget_item_id=%d amount=%.2f", item.ID, req.Amount))
	return record, nil
}

// List 分页查询支出记录。
func (s *ExpenseService) List(ctx context.Context, filter dto.ExpenseFilter) ([]model.ExpenseRecord, int64, error) {
	records, total, err := s.repo.List(ctx, repository.ExpenseListFilter{
		Status:       filter.Status,
		BudgetItemID: filter.BudgetItemID,
		SupplierID:   filter.SupplierID,
		Page:         filter.Page,
		PageSize:     filter.PageSize,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list expense records: %w", err)
	}
	return records, total, nil
}

// Get 获取支出记录。
func (s *ExpenseService) Get(ctx context.Context, id uint) (*model.ExpenseRecord, error) {
	record, err := s.repo.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get expense record %d: %w", id, err)
	}
	return record, nil
}

// Submit 提交支出：在单个事务内原子占用分项额度与整表余额。
// 分项已支出 + 审批中占用 + 本笔金额超过分项预算时返回冲突，
// 事务回滚保证支出状态与各项金额保持不变。
func (s *ExpenseService) Submit(ctx context.Context, actor model.Actor, id uint) (*model.ExpenseRecord, error) {
	record, item, budget, err := s.loadForTransition(ctx, id, constants.ExpenseStatusDraft, "submit")
	if err != nil {
		return nil, err
	}

	err = s.tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := s.itemRepo.TryFreeze(txCtx, item.ID, record.Amount); err != nil {
			return mapQuotaError(err, ErrItemQuotaExceeded, "freeze budget item %d", item.ID)
		}
		if err := s.budgetRepo.TryFreeze(txCtx, budget.ID, record.Amount); err != nil {
			return mapQuotaError(err, ErrInsufficientBalance, "freeze budget sheet %d", budget.ID)
		}
		if err := s.repo.UpdateStatus(txCtx, record.ID, constants.ExpenseStatusDraft, constants.ExpenseStatusSubmitted, nil); err != nil {
			return mapTransitionError(err, "submit expense record %d", record.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	record.Status = constants.ExpenseStatusSubmitted
	s.invalidateBudget(ctx, budget.ID)
	s.audit.Record(ctx, actor, "expense_submit", "expense", id, fmt.Sprintf("amount=%.2f budget_sheet_id=%d", record.Amount, budget.ID))
	return record, nil
}

// Approve 审批通过支出：在单个事务内把分项占用与整表冻结转为已支出。
func (s *ExpenseService) Approve(ctx context.Context, actor model.Actor, id uint, req dto.ApproveExpenseRequest) (*model.ExpenseRecord, error) {
	record, item, budget, err := s.loadForTransition(ctx, id, constants.ExpenseStatusSubmitted, "approve")
	if err != nil {
		return nil, err
	}

	fields := map[string]any{
		"approved_by_id":   actor.UserID,
		"approval_comment": req.ApprovalComment,
	}
	err = s.tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repo.UpdateStatus(txCtx, record.ID, constants.ExpenseStatusSubmitted, constants.ExpenseStatusApproved, fields); err != nil {
			return mapTransitionError(err, "approve expense record %d", record.ID)
		}
		if err := s.itemRepo.ConvertFreezeToSpent(txCtx, item.ID, record.Amount); err != nil {
			return mapTransitionError(err, "convert budget item %d freeze to spent", item.ID)
		}
		if err := s.budgetRepo.ConvertFreezeToSpent(txCtx, budget.ID, record.Amount); err != nil {
			return mapTransitionError(err, "convert budget sheet %d freeze to spent", budget.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	record.Status = constants.ExpenseStatusApproved
	record.ApprovedByID = &actor.UserID
	record.ApprovalComment = req.ApprovalComment
	s.invalidateBudget(ctx, budget.ID)
	s.audit.Record(ctx, actor, "expense_approve", "expense", id, fmt.Sprintf("amount=%.2f comment=%s", record.Amount, req.ApprovalComment))
	return record, nil
}

// Reject 驳回支出：在单个事务内释放分项占用与整表冻结。
func (s *ExpenseService) Reject(ctx context.Context, actor model.Actor, id uint, req dto.RejectExpenseRequest) (*model.ExpenseRecord, error) {
	record, item, budget, err := s.loadForTransition(ctx, id, constants.ExpenseStatusSubmitted, "reject")
	if err != nil {
		return nil, err
	}

	fields := map[string]any{
		"approved_by_id":   actor.UserID,
		"approval_comment": req.ApprovalComment,
	}
	err = s.tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := s.repo.UpdateStatus(txCtx, record.ID, constants.ExpenseStatusSubmitted, constants.ExpenseStatusRejected, fields); err != nil {
			return mapTransitionError(err, "reject expense record %d", record.ID)
		}
		if err := s.itemRepo.ReleaseFreeze(txCtx, item.ID, record.Amount); err != nil {
			return mapTransitionError(err, "release budget item %d freeze", item.ID)
		}
		if err := s.budgetRepo.ReleaseFreeze(txCtx, budget.ID, record.Amount); err != nil {
			return mapTransitionError(err, "release budget sheet %d freeze", budget.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	record.Status = constants.ExpenseStatusRejected
	record.ApprovedByID = &actor.UserID
	record.ApprovalComment = req.ApprovalComment
	s.invalidateBudget(ctx, budget.ID)
	s.audit.Record(ctx, actor, "expense_reject", "expense", id, fmt.Sprintf("amount=%.2f comment=%s", record.Amount, req.ApprovalComment))
	return record, nil
}

// Pay 确认付款，仅允许已审批通过的支出。
func (s *ExpenseService) Pay(ctx context.Context, actor model.Actor, id uint, req dto.PayExpenseRequest) (*model.ExpenseRecord, error) {
	record, err := s.repo.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get expense record %d: %w", id, err)
	}
	if record.Status != constants.ExpenseStatusApproved {
		return nil, fmt.Errorf("pay expense record %d: %w", id, ErrInvalidState)
	}
	paymentDate := time.Now()
	if req.PaymentDate != "" {
		parsed, err := time.Parse("2006-01-02", req.PaymentDate)
		if err != nil {
			return nil, fmt.Errorf("parse payment date: %w", err)
		}
		paymentDate = parsed
	}

	err = s.tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		fields := map[string]any{"payment_date": paymentDate}
		if err := s.repo.UpdateStatus(txCtx, record.ID, constants.ExpenseStatusApproved, constants.ExpenseStatusPaid, fields); err != nil {
			return mapTransitionError(err, "pay expense record %d", record.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	record.Status = constants.ExpenseStatusPaid
	record.PaymentDate = &paymentDate
	s.audit.Record(ctx, actor, "expense_pay", "expense", id, fmt.Sprintf("amount=%.2f", record.Amount))
	return record, nil
}

// loadForTransition 加载支出记录及其分项、预算表，并校验当前状态允许流转。
func (s *ExpenseService) loadForTransition(ctx context.Context, id uint, want constants.ExpenseStatus, op string) (*model.ExpenseRecord, *model.BudgetItem, *model.BudgetSheet, error) {
	record, err := s.repo.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, nil, ErrNotFound
		}
		return nil, nil, nil, fmt.Errorf("get expense record %d: %w", id, err)
	}
	if record.Status != want {
		return nil, nil, nil, fmt.Errorf("%s expense record %d: %w", op, id, ErrInvalidState)
	}
	item, err := s.itemRepo.FindByID(ctx, record.BudgetItemID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, nil, ErrNotFound
		}
		return nil, nil, nil, fmt.Errorf("get budget item %d: %w", record.BudgetItemID, err)
	}
	budget, err := s.budgetRepo.FindByID(ctx, item.BudgetSheetID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil, nil, ErrNotFound
		}
		return nil, nil, nil, fmt.Errorf("get budget sheet %d: %w", item.BudgetSheetID, err)
	}
	return record, item, budget, nil
}

// mapQuotaError 将仓储层额度错误映射为指定的服务层哨兵错误。
func mapQuotaError(err error, sentinel error, format string, args ...any) error {
	switch {
	case errors.Is(err, repository.ErrQuotaExceeded):
		return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), sentinel)
	default:
		return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), err)
	}
}

// mapTransitionError 将仓储层并发/状态错误映射为服务层哨兵错误。
func mapTransitionError(err error, format string, args ...any) error {
	switch {
	case errors.Is(err, repository.ErrConcurrentUpdate):
		return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrInvalidState)
	default:
		return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), err)
	}
}

func (s *ExpenseService) invalidateBudget(ctx context.Context, budgetSheetID uint) {
	if s.rdb == nil {
		return
	}
	if err := s.rdb.Del(ctx, budgetSnapshotKey(budgetSheetID)).Err(); err != nil {
		s.logger.Warn("invalidate budget snapshot failed", slog.Uint64("budget_id", uint64(budgetSheetID)), slog.String("error", err.Error()))
	}
}
