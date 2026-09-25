package repository

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/renovation/renovation-budget-api/internal/constants"
	"github.com/renovation/renovation-budget-api/internal/model"
)

// TestItemRepositoryReserveFrozenConcurrent 并发占用同一分项额度：
// 条件 UPDATE 必须保证超额的提交被拒绝，总额不被用穿。
func TestItemRepositoryReserveFrozenConcurrent(t *testing.T) {
	ctx := context.Background()
	db := newFileTestDB(t)
	txManager := NewTransactionManager(db)
	itemRepo := NewItemRepository(db)
	budgetRepo := NewBudgetRepository(db)

	sheet := &model.BudgetSheet{ProjectID: "p-c", Name: "预算", TotalAmount: 10000, AvailableAmount: 10000, Status: constants.BudgetStatusActive, CreatedByID: 1, Version: 1}
	if err := budgetRepo.Create(ctx, sheet); err != nil {
		t.Fatalf("create sheet: %v", err)
	}
	item := &model.BudgetItem{BudgetSheetID: sheet.ID, Category: constants.BudgetCategoryMaterial, BudgetAmount: 1000, AvailableAmount: 1000, VarianceAmount: -1000}
	if err := itemRepo.Create(ctx, item); err != nil {
		t.Fatalf("create item: %v", err)
	}

	const goroutines = 5
	const amount = 300.0 // 3 笔可成功（900），第 4 笔起必须冲突。
	errs := make([]error, goroutines)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			err := txManager.WithinTransaction(ctx, func(txCtx context.Context) error {
				return itemRepo.ReserveFrozen(txCtx, item.ID, amount)
			})
			errs[idx] = err
		}(i)
	}
	close(start)
	wg.Wait()

	var success, conflict int
	for _, err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrBudgetConflict):
			conflict++
		default:
			t.Fatalf("unexpected err: %v", err)
		}
	}
	if success != 3 || conflict != 2 {
		t.Fatalf("success=%d conflict=%d, want 3/2", success, conflict)
	}

	got, err := itemRepo.FindByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("find item: %v", err)
	}
	if got.FrozenAmount != 900 || got.AvailableAmount != 100 || got.SpentAmount != 0 {
		t.Fatalf("item spent=%v frozen=%v available=%v", got.SpentAmount, got.FrozenAmount, got.AvailableAmount)
	}
}

// TestExpenseRepositoryTransitionStatusCAS 状态只能沿期望的前置状态流转一次。
func TestExpenseRepositoryTransitionStatusCAS(t *testing.T) {
	ctx := context.Background()
	db := newFileTestDB(t)
	itemRepo := NewItemRepository(db)
	budgetRepo := NewBudgetRepository(db)
	expenseRepo := NewExpenseRepository(db)

	sheet := &model.BudgetSheet{ProjectID: "p-c", Name: "预算", TotalAmount: 1000, AvailableAmount: 1000, Status: constants.BudgetStatusActive, CreatedByID: 1, Version: 1}
	if err := budgetRepo.Create(ctx, sheet); err != nil {
		t.Fatalf("create sheet: %v", err)
	}
	item := &model.BudgetItem{BudgetSheetID: sheet.ID, Category: constants.BudgetCategoryMaterial, BudgetAmount: 1000, AvailableAmount: 1000, VarianceAmount: -1000}
	if err := itemRepo.Create(ctx, item); err != nil {
		t.Fatalf("create item: %v", err)
	}
	record := &model.ExpenseRecord{BudgetItemID: item.ID, Amount: 100, Status: constants.ExpenseStatusDraft, ApplicantID: 1}
	if err := expenseRepo.Create(ctx, record); err != nil {
		t.Fatalf("create: %v", err)
	}
	approver := uint(9)
	patch := ExpenseStatusPatch{Status: constants.ExpenseStatusApproved, ApprovedByID: &approver, ApprovalComment: "ok"}
	if err := expenseRepo.TransitionStatus(ctx, record.ID, constants.ExpenseStatusSubmitted, patch); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("from Draft err = %v, want ErrInvalidState", err)
	}
	if err := expenseRepo.TransitionStatus(ctx, record.ID, constants.ExpenseStatusDraft, ExpenseStatusPatch{Status: constants.ExpenseStatusSubmitted}); err != nil {
		t.Fatalf("draft -> submitted: %v", err)
	}
	if err := expenseRepo.TransitionStatus(ctx, record.ID, constants.ExpenseStatusSubmitted, patch); err != nil {
		t.Fatalf("submitted -> approved: %v", err)
	}
	// 重复审批必须失败，占用不会被二次结转。
	if err := expenseRepo.TransitionStatus(ctx, record.ID, constants.ExpenseStatusSubmitted, patch); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("double approve err = %v, want ErrInvalidState", err)
	}
	if err := expenseRepo.TransitionStatus(ctx, 99999, constants.ExpenseStatusSubmitted, patch); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing record err = %v, want ErrNotFound", err)
	}

	got, _ := expenseRepo.FindByID(ctx, record.ID)
	if got.Status != constants.ExpenseStatusApproved || got.ApprovedByID == nil || *got.ApprovedByID != approver {
		t.Fatalf("record = %+v", got)
	}
}
