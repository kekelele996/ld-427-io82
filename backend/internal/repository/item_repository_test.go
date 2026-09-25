package repository

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/renovation/renovation-budget-api/internal/constants"
	"github.com/renovation/renovation-budget-api/internal/model"
)

func TestItemRepositoryCRUD(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	budgetRepo := NewBudgetRepository(db)
	itemRepo := NewItemRepository(db)

	sheet := &model.BudgetSheet{ProjectID: "p-1", Name: "预算", TotalAmount: 1000, AvailableAmount: 1000, Status: constants.BudgetStatusDraft, CreatedByID: 1, Version: 1}
	if err := budgetRepo.Create(ctx, sheet); err != nil {
		t.Fatalf("create sheet: %v", err)
	}

	item := &model.BudgetItem{
		BudgetSheetID:  sheet.ID,
		Category:       constants.BudgetCategoryMaterial,
		SubCategory:    "瓷砖",
		BudgetAmount:   300,
		VarianceAmount: -300,
	}
	if err := itemRepo.Create(ctx, item); err != nil {
		t.Fatalf("create item: %v", err)
	}
	if item.ID == 0 {
		t.Fatalf("expected item id")
	}

	items, err := itemRepo.ListByBudgetID(ctx, sheet.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len = %d, want 1", len(items))
	}

	item.SpentAmount = 100
	item.VarianceAmount = -200
	if err := itemRepo.Update(ctx, item); err != nil {
		t.Fatalf("update item: %v", err)
	}
	got, err := itemRepo.FindByID(ctx, item.ID)
	if err != nil {
		t.Fatalf("find item: %v", err)
	}
	if got.SpentAmount != 100 {
		t.Fatalf("spent = %v, want 100", got.SpentAmount)
	}

	if err := itemRepo.Delete(ctx, item.ID); err != nil {
		t.Fatalf("delete item: %v", err)
	}
	if _, err := itemRepo.FindByID(ctx, item.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestItemRepositoryFreezeFlow(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	budgetRepo := NewBudgetRepository(db)
	itemRepo := NewItemRepository(db)

	sheet := &model.BudgetSheet{ProjectID: "p-1", Name: "预算", TotalAmount: 5000, AvailableAmount: 5000, Status: constants.BudgetStatusDraft, CreatedByID: 1, Version: 1}
	if err := budgetRepo.Create(ctx, sheet); err != nil {
		t.Fatalf("create sheet: %v", err)
	}
	item := &model.BudgetItem{BudgetSheetID: sheet.ID, Category: constants.BudgetCategoryMaterial, BudgetAmount: 1000, AvailableAmount: 1000, VarianceAmount: -1000}
	if err := itemRepo.Create(ctx, item); err != nil {
		t.Fatalf("create item: %v", err)
	}

	// 占用 400：可用 1000 -> 600。
	if err := itemRepo.TryFreeze(ctx, item.ID, 400); err != nil {
		t.Fatalf("try freeze 400: %v", err)
	}
	got, _ := itemRepo.FindByID(ctx, item.ID)
	if got.FrozenAmount != 400 || got.AvailableAmount != 600 {
		t.Fatalf("frozen=%v available=%v, want 400/600", got.FrozenAmount, got.AvailableAmount)
	}

	// 再占用 700 超出可用额度，必须失败且金额不变。
	if err := itemRepo.TryFreeze(ctx, item.ID, 700); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("try freeze 700 error = %v, want ErrQuotaExceeded", err)
	}
	got, _ = itemRepo.FindByID(ctx, item.ID)
	if got.FrozenAmount != 400 || got.AvailableAmount != 600 {
		t.Fatalf("after conflict frozen=%v available=%v, want 400/600", got.FrozenAmount, got.AvailableAmount)
	}

	// 审批通过：占用 400 转为已支出。
	if err := itemRepo.ConvertFreezeToSpent(ctx, item.ID, 400); err != nil {
		t.Fatalf("convert freeze to spent: %v", err)
	}
	got, _ = itemRepo.FindByID(ctx, item.ID)
	if got.FrozenAmount != 0 || got.SpentAmount != 400 || got.AvailableAmount != 600 || got.VarianceAmount != -600 {
		t.Fatalf("frozen=%v spent=%v available=%v variance=%v, want 0/400/600/-600",
			got.FrozenAmount, got.SpentAmount, got.AvailableAmount, got.VarianceAmount)
	}

	// 占用不足时释放必须失败。
	if err := itemRepo.ReleaseFreeze(ctx, item.ID, 100); !errors.Is(err, ErrConcurrentUpdate) {
		t.Fatalf("release without freeze error = %v, want ErrConcurrentUpdate", err)
	}

	// 占用 600 后驳回释放，可用恢复。
	if err := itemRepo.TryFreeze(ctx, item.ID, 600); err != nil {
		t.Fatalf("try freeze 600: %v", err)
	}
	if err := itemRepo.ReleaseFreeze(ctx, item.ID, 600); err != nil {
		t.Fatalf("release freeze: %v", err)
	}
	got, _ = itemRepo.FindByID(ctx, item.ID)
	if got.FrozenAmount != 0 || got.AvailableAmount != 600 {
		t.Fatalf("after release frozen=%v available=%v, want 0/600", got.FrozenAmount, got.AvailableAmount)
	}

	// 不存在的分项。
	if err := itemRepo.TryFreeze(ctx, 9999, 100); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("freeze missing item error = %v, want ErrQuotaExceeded", err)
	}
}

// TestItemRepositoryTryFreezeConcurrent 并发占用同一分项，最终占用不得超过分项预算。
func TestItemRepositoryTryFreezeConcurrent(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1) // sqlite 串行执行；原子性由单条条件 UPDATE 保证

	budgetRepo := NewBudgetRepository(db)
	itemRepo := NewItemRepository(db)
	sheet := &model.BudgetSheet{ProjectID: "p-1", Name: "预算", TotalAmount: 10000, AvailableAmount: 10000, Status: constants.BudgetStatusDraft, CreatedByID: 1, Version: 1}
	if err := budgetRepo.Create(ctx, sheet); err != nil {
		t.Fatalf("create sheet: %v", err)
	}
	item := &model.BudgetItem{BudgetSheetID: sheet.ID, Category: constants.BudgetCategoryLabor, BudgetAmount: 1000, AvailableAmount: 1000, VarianceAmount: -1000}
	if err := itemRepo.Create(ctx, item); err != nil {
		t.Fatalf("create item: %v", err)
	}

	const workers = 10
	const amount = 150.0
	var wg sync.WaitGroup
	var succeeded atomic.Int64
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := itemRepo.TryFreeze(ctx, item.ID, amount); err == nil {
				succeeded.Add(1)
			} else if !errors.Is(err, ErrQuotaExceeded) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	// 1000 / 150 = 6 笔成功，第 7 笔起必须冲突。
	if succeeded.Load() != 6 {
		t.Fatalf("succeeded = %d, want 6", succeeded.Load())
	}
	got, _ := itemRepo.FindByID(ctx, item.ID)
	if got.FrozenAmount != 900 || got.AvailableAmount != 100 {
		t.Fatalf("frozen=%v available=%v, want 900/100", got.FrozenAmount, got.AvailableAmount)
	}
}
