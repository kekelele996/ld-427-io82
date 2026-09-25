package service

import (
	"context"
	"errors"
	"testing"

	"github.com/renovation/renovation-budget-api/internal/constants"
	"github.com/renovation/renovation-budget-api/internal/dto"
	"github.com/renovation/renovation-budget-api/internal/model"
)

func newExpenseTestEnv() (*ExpenseService, *fakeBudgetRepo, *fakeItemRepo, *fakeExpenseRepo) {
	audit := NewAuditService(newFakeAuditRepo(), testLogger())
	budgetRepo := newFakeBudgetRepo()
	itemRepo := newFakeItemRepo()
	expenseRepo := newFakeExpenseRepo()
	svc := NewExpenseService(expenseRepo, itemRepo, budgetRepo, fakeTransactor{}, audit, nil, testLogger())
	return svc, budgetRepo, itemRepo, expenseRepo
}

func TestExpenseServiceFullFlow(t *testing.T) {
	ctx := context.Background()
	audit := NewAuditService(newFakeAuditRepo(), testLogger())
	budgetRepo := newFakeBudgetRepo()
	itemRepo := newFakeItemRepo()
	budgetSvc := NewBudgetService(budgetRepo, itemRepo, audit, nil, testLogger())
	expenseSvc := NewExpenseService(newFakeExpenseRepo(), itemRepo, budgetRepo, fakeTransactor{}, audit, nil, testLogger())

	sheet, err := budgetSvc.Create(ctx, model.Actor{UserID: 1, Username: "project"}, dto.CreateBudgetRequest{ProjectID: "p-1", Name: "整屋装修", TotalAmount: 10000})
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	item, err := NewItemService(itemRepo, budgetRepo, audit, nil, testLogger()).Create(ctx, model.Actor{UserID: 1}, sheet.ID, dto.CreateItemRequest{Category: constants.BudgetCategoryMaterial, BudgetAmount: 5000})
	if err != nil {
		t.Fatalf("create item: %v", err)
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
		t.Fatalf("after submit frozen=%v available=%v", budget.FrozenAmount, budget.AvailableAmount)
	}
	submittedItem, _ := itemRepo.FindByID(ctx, item.ID)
	if submittedItem.FrozenAmount != 500 || submittedItem.AvailableAmount != 4500 {
		t.Fatalf("after submit item frozen=%v available=%v", submittedItem.FrozenAmount, submittedItem.AvailableAmount)
	}

	if _, err := expenseSvc.Approve(ctx, model.Actor{UserID: 3, Username: "finance"}, record.ID, dto.ApproveExpenseRequest{ApprovalComment: "同意"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	budget, _ = budgetRepo.FindByID(ctx, sheet.ID)
	updatedItem, _ := itemRepo.FindByID(ctx, item.ID)
	if budget.FrozenAmount != 0 || budget.SpentAmount != 500 || budget.AvailableAmount != 9500 {
		t.Fatalf("after approve frozen=%v spent=%v available=%v", budget.FrozenAmount, budget.SpentAmount, budget.AvailableAmount)
	}
	if updatedItem.SpentAmount != 500 || updatedItem.VarianceAmount != -4500 {
		t.Fatalf("item spent=%v variance=%v", updatedItem.SpentAmount, updatedItem.VarianceAmount)
	}
	if updatedItem.FrozenAmount != 0 || updatedItem.AvailableAmount != 4500 {
		t.Fatalf("after approve item frozen=%v available=%v", updatedItem.FrozenAmount, updatedItem.AvailableAmount)
	}

	paid, err := expenseSvc.Pay(ctx, model.Actor{UserID: 3}, record.ID, dto.PayExpenseRequest{PaymentDate: "2026-08-10"})
	if err != nil {
		t.Fatalf("pay: %v", err)
	}
	if paid.Status != constants.ExpenseStatusPaid {
		t.Fatalf("status = %q, want Paid", paid.Status)
	}
}

func TestExpenseServiceSubmitInsufficient(t *testing.T) {
	ctx := context.Background()
	expenseSvc, budgetRepo, itemRepo, _ := newExpenseTestEnv()

	sheet := &model.BudgetSheet{ProjectID: "p-1", Name: "预算", TotalAmount: 100, AvailableAmount: 100, Status: constants.BudgetStatusDraft, CreatedByID: 1, Version: 1}
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
	if _, err := expenseSvc.Submit(ctx, model.Actor{UserID: 1}, record.ID); !errors.Is(err, ErrItemQuotaExceeded) {
		t.Fatalf("error = %v, want ErrItemQuotaExceeded", err)
	}
}

// TestExpenseServiceItemQuotaConflict 分项已支出+占用+本笔超过分项预算时返回冲突，
// 且支出状态与分项、整表各项金额都保持不变。
func TestExpenseServiceItemQuotaConflict(t *testing.T) {
	ctx := context.Background()
	expenseSvc, budgetRepo, itemRepo, expenseRepo := newExpenseTestEnv()

	sheet := &model.BudgetSheet{ProjectID: "p-1", Name: "预算", TotalAmount: 10000, AvailableAmount: 10000, Status: constants.BudgetStatusActive, CreatedByID: 1, Version: 1}
	if err := budgetRepo.Create(ctx, sheet); err != nil {
		t.Fatalf("create sheet: %v", err)
	}
	item := &model.BudgetItem{BudgetSheetID: sheet.ID, Category: constants.BudgetCategoryMaterial, BudgetAmount: 1000, AvailableAmount: 1000, VarianceAmount: -1000}
	if err := itemRepo.Create(ctx, item); err != nil {
		t.Fatalf("create item: %v", err)
	}

	first, err := expenseSvc.Create(ctx, model.Actor{UserID: 2}, dto.CreateExpenseRequest{BudgetItemID: item.ID, Amount: 600, ExpenseDate: "2026-08-01", PaymentMethod: constants.PaymentMethodCash})
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := expenseSvc.Submit(ctx, model.Actor{UserID: 2}, first.ID); err != nil {
		t.Fatalf("submit first: %v", err)
	}

	// 第二笔 500：600(占用) + 500(本笔) > 1000(分项预算)，必须冲突。
	second, err := expenseSvc.Create(ctx, model.Actor{UserID: 3}, dto.CreateExpenseRequest{BudgetItemID: item.ID, Amount: 500, ExpenseDate: "2026-08-02", PaymentMethod: constants.PaymentMethodCash})
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, err := expenseSvc.Submit(ctx, model.Actor{UserID: 3}, second.ID); !errors.Is(err, ErrItemQuotaExceeded) {
		t.Fatalf("error = %v, want ErrItemQuotaExceeded", err)
	}

	// 冲突后一切保持不变。
	got, _ := expenseRepo.FindByID(ctx, second.ID)
	if got.Status != constants.ExpenseStatusDraft {
		t.Fatalf("second status = %q, want Draft", got.Status)
	}
	gotItem, _ := itemRepo.FindByID(ctx, item.ID)
	if gotItem.FrozenAmount != 600 || gotItem.SpentAmount != 0 || gotItem.AvailableAmount != 400 {
		t.Fatalf("item frozen=%v spent=%v available=%v, want 600/0/400", gotItem.FrozenAmount, gotItem.SpentAmount, gotItem.AvailableAmount)
	}
	gotSheet, _ := budgetRepo.FindByID(ctx, sheet.ID)
	if gotSheet.FrozenAmount != 600 || gotSheet.AvailableAmount != 9400 {
		t.Fatalf("sheet frozen=%v available=%v, want 600/9400", gotSheet.FrozenAmount, gotSheet.AvailableAmount)
	}
}

// TestExpenseServiceApproveConvertsItemFreeze 审批通过把分项占用转为已支出。
func TestExpenseServiceApproveConvertsItemFreeze(t *testing.T) {
	ctx := context.Background()
	expenseSvc, budgetRepo, itemRepo, _ := newExpenseTestEnv()

	sheet := &model.BudgetSheet{ProjectID: "p-1", Name: "预算", TotalAmount: 10000, AvailableAmount: 10000, Status: constants.BudgetStatusActive, CreatedByID: 1, Version: 1}
	if err := budgetRepo.Create(ctx, sheet); err != nil {
		t.Fatalf("create sheet: %v", err)
	}
	item := &model.BudgetItem{BudgetSheetID: sheet.ID, Category: constants.BudgetCategoryLabor, BudgetAmount: 1000, AvailableAmount: 1000, VarianceAmount: -1000}
	if err := itemRepo.Create(ctx, item); err != nil {
		t.Fatalf("create item: %v", err)
	}
	record, err := expenseSvc.Create(ctx, model.Actor{UserID: 2}, dto.CreateExpenseRequest{BudgetItemID: item.ID, Amount: 300, ExpenseDate: "2026-08-01", PaymentMethod: constants.PaymentMethodBankTransfer})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := expenseSvc.Submit(ctx, model.Actor{UserID: 2}, record.ID); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := expenseSvc.Approve(ctx, model.Actor{UserID: 3}, record.ID, dto.ApproveExpenseRequest{ApprovalComment: "同意"}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	gotItem, _ := itemRepo.FindByID(ctx, item.ID)
	if gotItem.FrozenAmount != 0 || gotItem.SpentAmount != 300 || gotItem.AvailableAmount != 700 || gotItem.VarianceAmount != -700 {
		t.Fatalf("item frozen=%v spent=%v available=%v variance=%v, want 0/300/700/-700",
			gotItem.FrozenAmount, gotItem.SpentAmount, gotItem.AvailableAmount, gotItem.VarianceAmount)
	}
	gotSheet, _ := budgetRepo.FindByID(ctx, sheet.ID)
	if gotSheet.FrozenAmount != 0 || gotSheet.SpentAmount != 300 || gotSheet.AvailableAmount != 9700 {
		t.Fatalf("sheet frozen=%v spent=%v available=%v, want 0/300/9700", gotSheet.FrozenAmount, gotSheet.SpentAmount, gotSheet.AvailableAmount)
	}
}

// TestExpenseServiceRejectReleasesItemFreeze 驳回释放分项占用。
func TestExpenseServiceRejectReleasesItemFreeze(t *testing.T) {
	ctx := context.Background()
	expenseSvc, budgetRepo, itemRepo, _ := newExpenseTestEnv()

	sheet := &model.BudgetSheet{ProjectID: "p-1", Name: "预算", TotalAmount: 10000, AvailableAmount: 10000, Status: constants.BudgetStatusActive, CreatedByID: 1, Version: 1}
	if err := budgetRepo.Create(ctx, sheet); err != nil {
		t.Fatalf("create sheet: %v", err)
	}
	item := &model.BudgetItem{BudgetSheetID: sheet.ID, Category: constants.BudgetCategoryFurniture, BudgetAmount: 1000, AvailableAmount: 1000, VarianceAmount: -1000}
	if err := itemRepo.Create(ctx, item); err != nil {
		t.Fatalf("create item: %v", err)
	}
	record, err := expenseSvc.Create(ctx, model.Actor{UserID: 2}, dto.CreateExpenseRequest{BudgetItemID: item.ID, Amount: 400, ExpenseDate: "2026-08-01", PaymentMethod: constants.PaymentMethodCredit})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := expenseSvc.Submit(ctx, model.Actor{UserID: 2}, record.ID); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := expenseSvc.Reject(ctx, model.Actor{UserID: 3}, record.ID, dto.RejectExpenseRequest{ApprovalComment: "超出需求"}); err != nil {
		t.Fatalf("reject: %v", err)
	}

	gotItem, _ := itemRepo.FindByID(ctx, item.ID)
	if gotItem.FrozenAmount != 0 || gotItem.SpentAmount != 0 || gotItem.AvailableAmount != 1000 {
		t.Fatalf("item frozen=%v spent=%v available=%v, want 0/0/1000", gotItem.FrozenAmount, gotItem.SpentAmount, gotItem.AvailableAmount)
	}
	gotSheet, _ := budgetRepo.FindByID(ctx, sheet.ID)
	if gotSheet.FrozenAmount != 0 || gotSheet.AvailableAmount != 10000 {
		t.Fatalf("sheet frozen=%v available=%v, want 0/10000", gotSheet.FrozenAmount, gotSheet.AvailableAmount)
	}
}

// TestExpenseServiceSequentialSubmitsRespectQuota 模拟两个项目经理先后提交同一分项，额度不能被用穿。
func TestExpenseServiceSequentialSubmitsRespectQuota(t *testing.T) {
	ctx := context.Background()
	expenseSvc, budgetRepo, itemRepo, _ := newExpenseTestEnv()

	sheet := &model.BudgetSheet{ProjectID: "p-1", Name: "预算", TotalAmount: 10000, AvailableAmount: 10000, Status: constants.BudgetStatusActive, CreatedByID: 1, Version: 1}
	if err := budgetRepo.Create(ctx, sheet); err != nil {
		t.Fatalf("create sheet: %v", err)
	}
	item := &model.BudgetItem{BudgetSheetID: sheet.ID, Category: constants.BudgetCategoryMaterial, BudgetAmount: 1000, AvailableAmount: 1000, VarianceAmount: -1000}
	if err := itemRepo.Create(ctx, item); err != nil {
		t.Fatalf("create item: %v", err)
	}

	pmA := model.Actor{UserID: 2, Username: "pm-a"}
	pmB := model.Actor{UserID: 3, Username: "pm-b"}
	expA, err := expenseSvc.Create(ctx, pmA, dto.CreateExpenseRequest{BudgetItemID: item.ID, Amount: 700, ExpenseDate: "2026-08-01", PaymentMethod: constants.PaymentMethodCash})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	expB, err := expenseSvc.Create(ctx, pmB, dto.CreateExpenseRequest{BudgetItemID: item.ID, Amount: 700, ExpenseDate: "2026-08-01", PaymentMethod: constants.PaymentMethodCash})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	if _, err := expenseSvc.Submit(ctx, pmA, expA.ID); err != nil {
		t.Fatalf("submit A: %v", err)
	}
	if _, err := expenseSvc.Submit(ctx, pmB, expB.ID); !errors.Is(err, ErrItemQuotaExceeded) {
		t.Fatalf("submit B error = %v, want ErrItemQuotaExceeded", err)
	}

	gotItem, _ := itemRepo.FindByID(ctx, item.ID)
	if gotItem.FrozenAmount != 700 || gotItem.AvailableAmount != 300 {
		t.Fatalf("item frozen=%v available=%v, want 700/300", gotItem.FrozenAmount, gotItem.AvailableAmount)
	}

	// A 审批通过后，已支出 700，B 仍然超支。
	if _, err := expenseSvc.Approve(ctx, model.Actor{UserID: 4}, expA.ID, dto.ApproveExpenseRequest{}); err != nil {
		t.Fatalf("approve A: %v", err)
	}
	if _, err := expenseSvc.Submit(ctx, pmB, expB.ID); !errors.Is(err, ErrItemQuotaExceeded) {
		t.Fatalf("submit B after approve error = %v, want ErrItemQuotaExceeded", err)
	}
	gotItem, _ = itemRepo.FindByID(ctx, item.ID)
	if gotItem.SpentAmount != 700 || gotItem.FrozenAmount != 0 || gotItem.AvailableAmount != 300 {
		t.Fatalf("item spent=%v frozen=%v available=%v, want 700/0/300", gotItem.SpentAmount, gotItem.FrozenAmount, gotItem.AvailableAmount)
	}
}

// TestExpenseServiceDoubleSubmitConflict 同一笔支出不能重复提交。
func TestExpenseServiceDoubleSubmitConflict(t *testing.T) {
	ctx := context.Background()
	expenseSvc, budgetRepo, itemRepo, _ := newExpenseTestEnv()

	sheet := &model.BudgetSheet{ProjectID: "p-1", Name: "预算", TotalAmount: 10000, AvailableAmount: 10000, Status: constants.BudgetStatusActive, CreatedByID: 1, Version: 1}
	if err := budgetRepo.Create(ctx, sheet); err != nil {
		t.Fatalf("create sheet: %v", err)
	}
	item := &model.BudgetItem{BudgetSheetID: sheet.ID, Category: constants.BudgetCategoryOther, BudgetAmount: 1000, AvailableAmount: 1000, VarianceAmount: -1000}
	if err := itemRepo.Create(ctx, item); err != nil {
		t.Fatalf("create item: %v", err)
	}
	record, err := expenseSvc.Create(ctx, model.Actor{UserID: 2}, dto.CreateExpenseRequest{BudgetItemID: item.ID, Amount: 100, ExpenseDate: "2026-08-01", PaymentMethod: constants.PaymentMethodCash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := expenseSvc.Submit(ctx, model.Actor{UserID: 2}, record.ID); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if _, err := expenseSvc.Submit(ctx, model.Actor{UserID: 2}, record.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second submit error = %v, want ErrInvalidState", err)
	}
	gotItem, _ := itemRepo.FindByID(ctx, item.ID)
	if gotItem.FrozenAmount != 100 {
		t.Fatalf("item frozen = %v, want 100 (no double freeze)", gotItem.FrozenAmount)
	}
}
