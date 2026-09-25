package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/renovation/renovation-budget-api/internal/constants"
	"github.com/renovation/renovation-budget-api/internal/dto"
	"github.com/renovation/renovation-budget-api/internal/model"
)

func newExpenseServiceForTest(expenseRepo *fakeExpenseRepo, itemRepo *fakeItemRepo, budgetRepo *fakeBudgetRepo) *ExpenseService {
	audit := NewAuditService(newFakeAuditRepo(), testLogger())
	return NewExpenseService(expenseRepo, itemRepo, budgetRepo, newFakeTxManager(), audit, nil, testLogger())
}

func TestExpenseServiceFullFlow(t *testing.T) {
	ctx := context.Background()
	audit := NewAuditService(newFakeAuditRepo(), testLogger())
	budgetRepo := newFakeBudgetRepo()
	itemRepo := newFakeItemRepo()
	budgetSvc := NewBudgetService(budgetRepo, itemRepo, audit, nil, testLogger())
	expenseSvc := newExpenseServiceForTest(newFakeExpenseRepo(), itemRepo, budgetRepo)

	sheet, err := budgetSvc.Create(ctx, model.Actor{UserID: 1, Username: "project"}, dto.CreateBudgetRequest{ProjectID: "p-1", Name: "整屋装修", TotalAmount: 10000})
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	item, err := NewItemService(itemRepo, budgetRepo, audit, nil, testLogger()).Create(ctx, model.Actor{UserID: 1}, sheet.ID, dto.CreateItemRequest{Category: constants.BudgetCategoryMaterial, BudgetAmount: 5000})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	if item.AvailableAmount != 5000 {
		t.Fatalf("new item available = %v, want 5000", item.AvailableAmount)
	}

	record, err := expenseSvc.Create(ctx, model.Actor{UserID: 2, Username: "accountant"}, dto.CreateExpenseRequest{
		BudgetItemID:  item.ID,
		Amount:        500,
		ExpenseDate:   "2026-08-01",
		PaymentMethod: constants.PaymentMethodBankTransfer,
	})
	if err != nil {
		t.Fatalf("create expense: %v", err)
	}
	if record.Status != constants.ExpenseStatusDraft {
		t.Fatalf("status = %q, want Draft", record.Status)
	}

	if _, err := expenseSvc.Submit(ctx, model.Actor{UserID: 2}, record.ID); err != nil {
		t.Fatalf("submit: %v", err)
	}
	budget, _ := budgetRepo.FindByID(ctx, sheet.ID)
	if budget.FrozenAmount != 500 || budget.AvailableAmount != 9500 {
		t.Fatalf("after submit sheet frozen=%v available=%v", budget.FrozenAmount, budget.AvailableAmount)
	}
	submittedItem, _ := itemRepo.FindByID(ctx, item.ID)
	if submittedItem.SpentAmount != 0 || submittedItem.FrozenAmount != 500 || submittedItem.AvailableAmount != 4500 {
		t.Fatalf("after submit item spent=%v frozen=%v available=%v", submittedItem.SpentAmount, submittedItem.FrozenAmount, submittedItem.AvailableAmount)
	}

	if _, err := expenseSvc.Approve(ctx, model.Actor{UserID: 3, Username: "finance"}, record.ID, dto.ApproveExpenseRequest{ApprovalComment: "同意"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	budget, _ = budgetRepo.FindByID(ctx, sheet.ID)
	updatedItem, _ := itemRepo.FindByID(ctx, item.ID)
	if budget.FrozenAmount != 0 || budget.SpentAmount != 500 || budget.AvailableAmount != 9500 {
		t.Fatalf("after approve frozen=%v spent=%v available=%v", budget.FrozenAmount, budget.SpentAmount, budget.AvailableAmount)
	}
	if updatedItem.SpentAmount != 500 || updatedItem.FrozenAmount != 0 || updatedItem.AvailableAmount != 4500 || updatedItem.VarianceAmount != -4500 {
		t.Fatalf("item spent=%v frozen=%v available=%v variance=%v", updatedItem.SpentAmount, updatedItem.FrozenAmount, updatedItem.AvailableAmount, updatedItem.VarianceAmount)
	}

	paid, err := expenseSvc.Pay(ctx, model.Actor{UserID: 3}, record.ID, dto.PayExpenseRequest{PaymentDate: "2026-08-10"})
	if err != nil {
		t.Fatalf("pay: %v", err)
	}
	if paid.Status != constants.ExpenseStatusPaid {
		t.Fatalf("status = %q, want Paid", paid.Status)
	}
}

func TestExpenseServiceSubmitItemLimitExceeded(t *testing.T) {
	ctx := context.Background()
	budgetRepo := newFakeBudgetRepo()
	itemRepo := newFakeItemRepo()
	expenseRepo := newFakeExpenseRepo()
	expenseSvc := newExpenseServiceForTest(expenseRepo, itemRepo, budgetRepo)

	// 整表额度 10000，但分项额度只有 100：提交必须按分项预算拦截。
	sheet := &model.BudgetSheet{ProjectID: "p-1", Name: "预算", TotalAmount: 10000, AvailableAmount: 10000, Status: constants.BudgetStatusActive, CreatedByID: 1, Version: 1}
	if err := budgetRepo.Create(ctx, sheet); err != nil {
		t.Fatalf("create sheet: %v", err)
	}
	item := &model.BudgetItem{BudgetSheetID: sheet.ID, Category: constants.BudgetCategoryLabor, BudgetAmount: 100, AvailableAmount: 100, VarianceAmount: -100}
	if err := itemRepo.Create(ctx, item); err != nil {
		t.Fatalf("create item: %v", err)
	}
	record, err := expenseSvc.Create(ctx, model.Actor{UserID: 1}, dto.CreateExpenseRequest{BudgetItemID: item.ID, Amount: 200, ExpenseDate: "2026-08-01", PaymentMethod: constants.PaymentMethodCash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := expenseSvc.Submit(ctx, model.Actor{UserID: 1}, record.ID); !errors.Is(err, ErrItemBudgetExceeded) {
		t.Fatalf("error = %v, want ErrItemBudgetExceeded", err)
	}

	// 冲突后支出状态与所有金额保持不变。
	unchanged, _ := expenseRepo.FindByID(ctx, record.ID)
	if unchanged.Status != constants.ExpenseStatusDraft {
		t.Fatalf("record status = %q, want Draft", unchanged.Status)
	}
	unchangedItem, _ := itemRepo.FindByID(ctx, item.ID)
	if unchangedItem.SpentAmount != 0 || unchangedItem.FrozenAmount != 0 || unchangedItem.AvailableAmount != 100 {
		t.Fatalf("item changed after conflict: spent=%v frozen=%v available=%v", unchangedItem.SpentAmount, unchangedItem.FrozenAmount, unchangedItem.AvailableAmount)
	}
	unchangedSheet, _ := budgetRepo.FindByID(ctx, sheet.ID)
	if unchangedSheet.FrozenAmount != 0 || unchangedSheet.SpentAmount != 0 || unchangedSheet.AvailableAmount != 10000 {
		t.Fatalf("sheet changed after conflict: frozen=%v spent=%v available=%v", unchangedSheet.FrozenAmount, unchangedSheet.SpentAmount, unchangedSheet.AvailableAmount)
	}
}

func TestExpenseServiceSubmitOccupiedLimit(t *testing.T) {
	ctx := context.Background()
	budgetRepo := newFakeBudgetRepo()
	itemRepo := newFakeItemRepo()
	expenseRepo := newFakeExpenseRepo()
	expenseSvc := newExpenseServiceForTest(expenseRepo, itemRepo, budgetRepo)

	sheet := &model.BudgetSheet{ProjectID: "p-1", Name: "预算", TotalAmount: 10000, AvailableAmount: 10000, Status: constants.BudgetStatusActive, CreatedByID: 1, Version: 1}
	if err := budgetRepo.Create(ctx, sheet); err != nil {
		t.Fatalf("create sheet: %v", err)
	}
	item := &model.BudgetItem{BudgetSheetID: sheet.ID, Category: constants.BudgetCategoryMaterial, BudgetAmount: 1000, AvailableAmount: 1000, VarianceAmount: -1000}
	if err := itemRepo.Create(ctx, item); err != nil {
		t.Fatalf("create item: %v", err)
	}

	// 第一笔 600 提交成功：占用 600，剩余可用 400。
	first, err := expenseSvc.Create(ctx, model.Actor{UserID: 1}, dto.CreateExpenseRequest{BudgetItemID: item.ID, Amount: 600, ExpenseDate: "2026-08-01", PaymentMethod: constants.PaymentMethodCash})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := expenseSvc.Submit(ctx, model.Actor{UserID: 1}, first.ID); err != nil {
		t.Fatalf("submit first: %v", err)
	}

	// 第二笔 500：已支出 0 + 占用 600 + 本笔 500 > 分项预算 1000，必须冲突。
	second, err := expenseSvc.Create(ctx, model.Actor{UserID: 1}, dto.CreateExpenseRequest{BudgetItemID: item.ID, Amount: 500, ExpenseDate: "2026-08-02", PaymentMethod: constants.PaymentMethodCash})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, err := expenseSvc.Submit(ctx, model.Actor{UserID: 1}, second.ID); !errors.Is(err, ErrItemBudgetExceeded) {
		t.Fatalf("error = %v, want ErrItemBudgetExceeded", err)
	}
	after, _ := itemRepo.FindByID(ctx, item.ID)
	if after.FrozenAmount != 600 || after.AvailableAmount != 400 {
		t.Fatalf("after second rejected: frozen=%v available=%v", after.FrozenAmount, after.AvailableAmount)
	}

	// 驳回第一笔后占用释放，可用额度恢复，第二笔可提交。
	if _, err := expenseSvc.Reject(ctx, model.Actor{UserID: 3}, first.ID, dto.RejectExpenseRequest{ApprovalComment: "暂缓"}); err != nil {
		t.Fatalf("reject first: %v", err)
	}
	released, _ := itemRepo.FindByID(ctx, item.ID)
	if released.FrozenAmount != 0 || released.AvailableAmount != 1000 {
		t.Fatalf("after reject: frozen=%v available=%v", released.FrozenAmount, released.AvailableAmount)
	}
	if _, err := expenseSvc.Submit(ctx, model.Actor{UserID: 1}, second.ID); err != nil {
		t.Fatalf("resubmit second: %v", err)
	}
}

// TestExpenseServiceConcurrentSubmit 两笔支出并发提交同一分项，总额超过预算时只能有一笔成功。
func TestExpenseServiceConcurrentSubmit(t *testing.T) {
	ctx := context.Background()
	budgetRepo := newFakeBudgetRepo()
	itemRepo := newFakeItemRepo()
	expenseRepo := newFakeExpenseRepo()
	expenseSvc := newExpenseServiceForTest(expenseRepo, itemRepo, budgetRepo)

	sheet := &model.BudgetSheet{ProjectID: "p-1", Name: "预算", TotalAmount: 10000, AvailableAmount: 10000, Status: constants.BudgetStatusActive, CreatedByID: 1, Version: 1}
	if err := budgetRepo.Create(ctx, sheet); err != nil {
		t.Fatalf("create sheet: %v", err)
	}
	item := &model.BudgetItem{BudgetSheetID: sheet.ID, Category: constants.BudgetCategoryMaterial, BudgetAmount: 1000, AvailableAmount: 1000, VarianceAmount: -1000}
	if err := itemRepo.Create(ctx, item); err != nil {
		t.Fatalf("create item: %v", err)
	}

	var recordIDs [2]uint
	for i := range recordIDs {
		record, err := expenseSvc.Create(ctx, model.Actor{UserID: 1}, dto.CreateExpenseRequest{BudgetItemID: item.ID, Amount: 600, ExpenseDate: "2026-08-01", PaymentMethod: constants.PaymentMethodCash})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		recordIDs[i] = record.ID
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	start := make(chan struct{})
	for i, id := range recordIDs {
		wg.Add(1)
		go func(idx, recordID uint) {
			defer wg.Done()
			<-start
			_, results[idx] = expenseSvc.Submit(ctx, model.Actor{UserID: 1}, recordID)
		}(uint(i), id)
	}
	close(start)
	wg.Wait()

	var success, conflict int
	for _, err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrItemBudgetExceeded):
			conflict++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d, want 1/1 (errors: %v, %v)", success, conflict, results[0], results[1])
	}

	finalItem, _ := itemRepo.FindByID(ctx, item.ID)
	if finalItem.FrozenAmount != 600 || finalItem.AvailableAmount != 400 || finalItem.SpentAmount != 0 {
		t.Fatalf("final item: spent=%v frozen=%v available=%v", finalItem.SpentAmount, finalItem.FrozenAmount, finalItem.AvailableAmount)
	}
	finalSheet, _ := budgetRepo.FindByID(ctx, sheet.ID)
	if finalSheet.FrozenAmount != 600 {
		t.Fatalf("final sheet frozen=%v, want 600", finalSheet.FrozenAmount)
	}
}

func TestExpenseServiceRejectReleasesOccupation(t *testing.T) {
	ctx := context.Background()
	budgetRepo := newFakeBudgetRepo()
	itemRepo := newFakeItemRepo()
	expenseSvc := newExpenseServiceForTest(newFakeExpenseRepo(), itemRepo, budgetRepo)

	sheet := &model.BudgetSheet{ProjectID: "p-1", Name: "预算", TotalAmount: 1000, AvailableAmount: 1000, Status: constants.BudgetStatusActive, CreatedByID: 1, Version: 1}
	if err := budgetRepo.Create(ctx, sheet); err != nil {
		t.Fatalf("create sheet: %v", err)
	}
	item := &model.BudgetItem{BudgetSheetID: sheet.ID, Category: constants.BudgetCategoryLabor, BudgetAmount: 100, AvailableAmount: 100, VarianceAmount: -100}
	if err := itemRepo.Create(ctx, item); err != nil {
		t.Fatalf("create item: %v", err)
	}
	record, err := expenseSvc.Create(ctx, model.Actor{UserID: 1}, dto.CreateExpenseRequest{BudgetItemID: item.ID, Amount: 100, ExpenseDate: "2026-08-01", PaymentMethod: constants.PaymentMethodCash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := expenseSvc.Submit(ctx, model.Actor{UserID: 1}, record.ID); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := expenseSvc.Reject(ctx, model.Actor{UserID: 3}, record.ID, dto.RejectExpenseRequest{ApprovalComment: "不合规"}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	rejected, _ := itemRepo.FindByID(ctx, item.ID)
	if rejected.FrozenAmount != 0 || rejected.SpentAmount != 0 || rejected.AvailableAmount != 100 {
		t.Fatalf("after reject item spent=%v frozen=%v available=%v", rejected.SpentAmount, rejected.FrozenAmount, rejected.AvailableAmount)
	}
	rejectedSheet, _ := budgetRepo.FindByID(ctx, sheet.ID)
	if rejectedSheet.FrozenAmount != 0 || rejectedSheet.AvailableAmount != 1000 {
		t.Fatalf("after reject sheet frozen=%v available=%v", rejectedSheet.FrozenAmount, rejectedSheet.AvailableAmount)
	}
}
