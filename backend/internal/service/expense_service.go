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
	txManager  repository.TransactionManager
	audit      *AuditService
	rdb        *redis.Client
	logger     *slog.Logger
}

// NewExpenseService 构造支出记录服务。
func NewExpenseService(repo repository.ExpenseRepository, itemRepo repository.ItemRepository, budgetRepo repository.BudgetRepository, txManager repository.TransactionManager, audit *AuditService, rdb *redis.Client, logger *slog.Logger) *ExpenseService {
	return &ExpenseService{repo: repo, itemRepo: itemRepo, budgetRepo: budgetRepo, txManager: txManager, audit: audit, rdb: rdb, logger: logger}
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

// Submit 提交支出并占用分项额度。
//
// 分项已支出 + 审批中占用 + 本笔金额超过分项预算时返回冲突，
// 支出状态与所有金额保持不变。校验与占用在单条条件 UPDATE 中完成，
// 同一分项并发提交由数据库行锁串行化，额度不会被用穿。
func (s *ExpenseService) Submit(ctx context.Context, actor model.Actor, id uint) (*model.ExpenseRecord, error) {
	record, err := s.repo.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get expense record %d: %w", id, err)
	}
	if record.Status != constants.ExpenseStatusDraft {
		return nil, fmt.Errorf("submit expense record %d: %w", id, ErrInvalidState)
	}
	item, err := s.itemRepo.FindByID(ctx, record.BudgetItemID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get budget item %d: %w", record.BudgetItemID, err)
	}

	err = s.txManager.WithinTransaction(ctx, func(txCtx context.Context) error {
		// 分项额度条件更新：budget_amount - spent_amount - frozen_amount >= amount。
		if txErr := s.itemRepo.ReserveFrozen(txCtx, item.ID, record.Amount); txErr != nil {
			if errors.Is(txErr, repository.ErrBudgetConflict) {
				return ErrItemBudgetExceeded
			}
			return fmt.Errorf("reserve budget item %d: %w", item.ID, txErr)
		}
		if txErr := s.budgetRepo.AdjustAmounts(txCtx, item.BudgetSheetID, repository.BudgetAmountPatch{
			FrozenDelta: record.Amount,
		}); txErr != nil {
			return fmt.Errorf("freeze budget sheet %d: %w", item.BudgetSheetID, txErr)
		}
		if txErr := s.repo.TransitionStatus(txCtx, id, constants.ExpenseStatusDraft, repository.ExpenseStatusPatch{
			Status: constants.ExpenseStatusSubmitted,
		}); txErr != nil {
			return mapTransitionError(id, "submit", txErr)
		}
		return nil
	})
	if err != nil {
		// 事务回滚后支出状态与各项金额均不变。
		return nil, fmt.Errorf("submit expense record %d: %w", id, err)
	}

	record.Status = constants.ExpenseStatusSubmitted
	s.invalidateBudget(ctx, item.BudgetSheetID)
	s.audit.Record(ctx, actor, "expense_submit", "expense", id, fmt.Sprintf("amount=%.2f budget_item_id=%d budget_sheet_id=%d", record.Amount, item.ID, item.BudgetSheetID))
	return record, nil
}

// Approve 审批通过支出，占用金额结转为已支出金额。
func (s *ExpenseService) Approve(ctx context.Context, actor model.Actor, id uint, req dto.ApproveExpenseRequest) (*model.ExpenseRecord, error) {
	record, err := s.repo.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get expense record %d: %w", id, err)
	}
	if record.Status != constants.ExpenseStatusSubmitted {
		return nil, fmt.Errorf("approve expense record %d: %w", id, ErrInvalidState)
	}
	item, err := s.itemRepo.FindByID(ctx, record.BudgetItemID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get budget item %d: %w", record.BudgetItemID, err)
	}
	approverID := actor.UserID

	err = s.txManager.WithinTransaction(ctx, func(txCtx context.Context) error {
		if txErr := s.itemRepo.ConfirmFrozen(txCtx, item.ID, record.Amount); txErr != nil {
			return fmt.Errorf("confirm budget item %d: %w", item.ID, txErr)
		}
		if txErr := s.budgetRepo.AdjustAmounts(txCtx, item.BudgetSheetID, repository.BudgetAmountPatch{
			SpentDelta:  record.Amount,
			FrozenDelta: -record.Amount,
		}); txErr != nil {
			return fmt.Errorf("settle budget sheet %d: %w", item.BudgetSheetID, txErr)
		}
		if txErr := s.repo.TransitionStatus(txCtx, id, constants.ExpenseStatusSubmitted, repository.ExpenseStatusPatch{
			Status:          constants.ExpenseStatusApproved,
			ApprovedByID:    &approverID,
			ApprovalComment: req.ApprovalComment,
		}); txErr != nil {
			return mapTransitionError(id, "approve", txErr)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("approve expense record %d: %w", id, err)
	}

	record.Status = constants.ExpenseStatusApproved
	record.ApprovedByID = &approverID
	record.ApprovalComment = req.ApprovalComment
	s.invalidateBudget(ctx, item.BudgetSheetID)
	s.audit.Record(ctx, actor, "expense_approve", "expense", id, fmt.Sprintf("amount=%.2f comment=%s", record.Amount, req.ApprovalComment))
	return record, nil
}

// Reject 驳回支出并释放占用金额。
func (s *ExpenseService) Reject(ctx context.Context, actor model.Actor, id uint, req dto.RejectExpenseRequest) (*model.ExpenseRecord, error) {
	record, err := s.repo.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get expense record %d: %w", id, err)
	}
	if record.Status != constants.ExpenseStatusSubmitted {
		return nil, fmt.Errorf("reject expense record %d: %w", id, ErrInvalidState)
	}
	item, err := s.itemRepo.FindByID(ctx, record.BudgetItemID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get budget item %d: %w", record.BudgetItemID, err)
	}
	approverID := actor.UserID

	err = s.txManager.WithinTransaction(ctx, func(txCtx context.Context) error {
		if txErr := s.itemRepo.ReleaseFrozen(txCtx, item.ID, record.Amount); txErr != nil {
			return fmt.Errorf("release budget item %d: %w", item.ID, txErr)
		}
		if txErr := s.budgetRepo.AdjustAmounts(txCtx, item.BudgetSheetID, repository.BudgetAmountPatch{
			FrozenDelta: -record.Amount,
		}); txErr != nil {
			return fmt.Errorf("unfreeze budget sheet %d: %w", item.BudgetSheetID, txErr)
		}
		if txErr := s.repo.TransitionStatus(txCtx, id, constants.ExpenseStatusSubmitted, repository.ExpenseStatusPatch{
			Status:          constants.ExpenseStatusRejected,
			ApprovedByID:    &approverID,
			ApprovalComment: req.ApprovalComment,
		}); txErr != nil {
			return mapTransitionError(id, "reject", txErr)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reject expense record %d: %w", id, err)
	}

	record.Status = constants.ExpenseStatusRejected
	record.ApprovedByID = &approverID
	record.ApprovalComment = req.ApprovalComment
	s.invalidateBudget(ctx, item.BudgetSheetID)
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
	record.Status = constants.ExpenseStatusPaid
	if req.PaymentDate != "" {
		paymentDate, err := time.Parse("2006-01-02", req.PaymentDate)
		if err != nil {
			return nil, fmt.Errorf("parse payment date: %w", err)
		}
		record.PaymentDate = &paymentDate
	} else {
		now := time.Now()
		record.PaymentDate = &now
	}
	if err := s.repo.Update(ctx, record); err != nil {
		return nil, fmt.Errorf("pay expense record %d: %w", id, err)
	}
	s.audit.Record(ctx, actor, "expense_pay", "expense", id, fmt.Sprintf("amount=%.2f", record.Amount))
	return record, nil
}

// mapTransitionError 把仓储层条件更新失败转换为服务层哨兵错误。
func mapTransitionError(id uint, action string, err error) error {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, repository.ErrInvalidState):
		return ErrInvalidState
	default:
		return fmt.Errorf("%s expense record %d: %w", action, id, err)
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
