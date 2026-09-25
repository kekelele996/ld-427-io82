package service

import (
	"context"
	"time"

	"github.com/renovation/renovation-budget-api/internal/constants"
	"github.com/renovation/renovation-budget-api/internal/model"
	"github.com/renovation/renovation-budget-api/internal/repository"
)

type fakeBudgetRepo struct {
	nextID uint
	items  map[uint]*model.BudgetSheet
}

func newFakeBudgetRepo() *fakeBudgetRepo {
	return &fakeBudgetRepo{nextID: 1, items: make(map[uint]*model.BudgetSheet)}
}

func (f *fakeBudgetRepo) Create(_ context.Context, sheet *model.BudgetSheet) error {
	sheet.ID = f.nextID
	f.nextID++
	cp := *sheet
	f.items[cp.ID] = &cp
	*sheet = cp
	return nil
}

func (f *fakeBudgetRepo) FindByID(_ context.Context, id uint) (*model.BudgetSheet, error) {
	v, ok := f.items[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *v
	return &cp, nil
}

func (f *fakeBudgetRepo) List(_ context.Context, filter repository.BudgetListFilter) ([]model.BudgetSheet, int64, error) {
	var out []model.BudgetSheet
	for _, v := range f.items {
		if filter.ProjectID != "" && v.ProjectID != filter.ProjectID {
			continue
		}
		if filter.Status != "" && string(v.Status) != filter.Status {
			continue
		}
		out = append(out, *v)
	}
	return out, int64(len(out)), nil
}

func (f *fakeBudgetRepo) Update(_ context.Context, sheet *model.BudgetSheet) error {
	cp := *sheet
	f.items[sheet.ID] = &cp
	return nil
}

func (f *fakeBudgetRepo) Delete(_ context.Context, id uint) error {
	delete(f.items, id)
	return nil
}

func (f *fakeBudgetRepo) TryFreeze(_ context.Context, sheetID uint, amount float64) error {
	sheet, ok := f.items[sheetID]
	if !ok {
		return repository.ErrNotFound
	}
	if sheet.TotalAmount-sheet.SpentAmount-sheet.FrozenAmount < amount {
		return repository.ErrQuotaExceeded
	}
	sheet.FrozenAmount += amount
	sheet.AvailableAmount = sheet.TotalAmount - sheet.SpentAmount - sheet.FrozenAmount
	return nil
}

func (f *fakeBudgetRepo) ReleaseFreeze(_ context.Context, sheetID uint, amount float64) error {
	sheet, ok := f.items[sheetID]
	if !ok {
		return repository.ErrNotFound
	}
	if sheet.FrozenAmount < amount {
		return repository.ErrConcurrentUpdate
	}
	sheet.FrozenAmount -= amount
	sheet.AvailableAmount = sheet.TotalAmount - sheet.SpentAmount - sheet.FrozenAmount
	return nil
}

func (f *fakeBudgetRepo) ConvertFreezeToSpent(_ context.Context, sheetID uint, amount float64) error {
	sheet, ok := f.items[sheetID]
	if !ok {
		return repository.ErrNotFound
	}
	if sheet.FrozenAmount < amount {
		return repository.ErrConcurrentUpdate
	}
	sheet.FrozenAmount -= amount
	sheet.SpentAmount += amount
	sheet.AvailableAmount = sheet.TotalAmount - sheet.SpentAmount - sheet.FrozenAmount
	return nil
}

type fakeItemRepo struct {
	nextID uint
	items  map[uint]*model.BudgetItem
}

func newFakeItemRepo() *fakeItemRepo {
	return &fakeItemRepo{nextID: 1, items: make(map[uint]*model.BudgetItem)}
}

func (f *fakeItemRepo) Create(_ context.Context, item *model.BudgetItem) error {
	item.ID = f.nextID
	f.nextID++
	cp := *item
	f.items[cp.ID] = &cp
	*item = cp
	return nil
}

func (f *fakeItemRepo) FindByID(_ context.Context, id uint) (*model.BudgetItem, error) {
	v, ok := f.items[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *v
	return &cp, nil
}

func (f *fakeItemRepo) ListByBudgetID(_ context.Context, budgetSheetID uint) ([]model.BudgetItem, error) {
	var out []model.BudgetItem
	for _, v := range f.items {
		if v.BudgetSheetID == budgetSheetID {
			out = append(out, *v)
		}
	}
	return out, nil
}

func (f *fakeItemRepo) Update(_ context.Context, item *model.BudgetItem) error {
	cp := *item
	f.items[item.ID] = &cp
	return nil
}

func (f *fakeItemRepo) Delete(_ context.Context, id uint) error {
	delete(f.items, id)
	return nil
}

func (f *fakeItemRepo) TryFreeze(_ context.Context, itemID uint, amount float64) error {
	item, ok := f.items[itemID]
	if !ok {
		return repository.ErrNotFound
	}
	if item.BudgetAmount-item.SpentAmount-item.FrozenAmount < amount {
		return repository.ErrQuotaExceeded
	}
	item.FrozenAmount += amount
	item.AvailableAmount = item.BudgetAmount - item.SpentAmount - item.FrozenAmount
	return nil
}

func (f *fakeItemRepo) ReleaseFreeze(_ context.Context, itemID uint, amount float64) error {
	item, ok := f.items[itemID]
	if !ok {
		return repository.ErrNotFound
	}
	if item.FrozenAmount < amount {
		return repository.ErrConcurrentUpdate
	}
	item.FrozenAmount -= amount
	item.AvailableAmount = item.BudgetAmount - item.SpentAmount - item.FrozenAmount
	return nil
}

func (f *fakeItemRepo) ConvertFreezeToSpent(_ context.Context, itemID uint, amount float64) error {
	item, ok := f.items[itemID]
	if !ok {
		return repository.ErrNotFound
	}
	if item.FrozenAmount < amount {
		return repository.ErrConcurrentUpdate
	}
	item.FrozenAmount -= amount
	item.SpentAmount += amount
	item.VarianceAmount = item.SpentAmount - item.BudgetAmount
	item.AvailableAmount = item.BudgetAmount - item.SpentAmount - item.FrozenAmount
	return nil
}

type fakeExpenseRepo struct {
	nextID  uint
	records map[uint]*model.ExpenseRecord
}

func newFakeExpenseRepo() *fakeExpenseRepo {
	return &fakeExpenseRepo{nextID: 1, records: make(map[uint]*model.ExpenseRecord)}
}

func (f *fakeExpenseRepo) Create(_ context.Context, record *model.ExpenseRecord) error {
	record.ID = f.nextID
	f.nextID++
	cp := *record
	f.records[cp.ID] = &cp
	*record = cp
	return nil
}

func (f *fakeExpenseRepo) FindByID(_ context.Context, id uint) (*model.ExpenseRecord, error) {
	v, ok := f.records[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *v
	return &cp, nil
}

func (f *fakeExpenseRepo) List(_ context.Context, filter repository.ExpenseListFilter) ([]model.ExpenseRecord, int64, error) {
	var out []model.ExpenseRecord
	for _, v := range f.records {
		if filter.Status != "" && string(v.Status) != filter.Status {
			continue
		}
		if filter.BudgetItemID != 0 && v.BudgetItemID != filter.BudgetItemID {
			continue
		}
		out = append(out, *v)
	}
	return out, int64(len(out)), nil
}

func (f *fakeExpenseRepo) Update(_ context.Context, record *model.ExpenseRecord) error {
	cp := *record
	f.records[record.ID] = &cp
	return nil
}

func (f *fakeExpenseRepo) UpdateStatus(_ context.Context, id uint, from, to constants.ExpenseStatus, fields map[string]any) error {
	record, ok := f.records[id]
	if !ok {
		return repository.ErrNotFound
	}
	if record.Status != from {
		return repository.ErrConcurrentUpdate
	}
	record.Status = to
	if v, ok := fields["approved_by_id"]; ok {
		if uid, ok := v.(uint); ok {
			record.ApprovedByID = &uid
		}
	}
	if v, ok := fields["approval_comment"]; ok {
		if comment, ok := v.(string); ok {
			record.ApprovalComment = comment
		}
	}
	if v, ok := fields["payment_date"]; ok {
		if ts, ok := v.(time.Time); ok {
			record.PaymentDate = &ts
		}
	}
	return nil
}

// fakeTransactor 直接执行函数，内存假仓储本身即原子。
type fakeTransactor struct{}

func (fakeTransactor) WithinTransaction(ctx context.Context, fn func(txCtx context.Context) error) error {
	return fn(ctx)
}

type fakeSupplierRepo struct {
	nextID    uint
	suppliers map[uint]*model.Supplier
}

func newFakeSupplierRepo() *fakeSupplierRepo {
	return &fakeSupplierRepo{nextID: 1, suppliers: make(map[uint]*model.Supplier)}
}

func (f *fakeSupplierRepo) Create(_ context.Context, supplier *model.Supplier) error {
	supplier.ID = f.nextID
	f.nextID++
	cp := *supplier
	f.suppliers[cp.ID] = &cp
	*supplier = cp
	return nil
}

func (f *fakeSupplierRepo) FindByID(_ context.Context, id uint) (*model.Supplier, error) {
	v, ok := f.suppliers[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *v
	return &cp, nil
}

func (f *fakeSupplierRepo) List(_ context.Context, filter repository.SupplierListFilter) ([]model.Supplier, int64, error) {
	var out []model.Supplier
	for _, v := range f.suppliers {
		if filter.Status != "" && string(v.Status) != filter.Status {
			continue
		}
		if filter.Category != "" && string(v.Category) != filter.Category {
			continue
		}
		out = append(out, *v)
	}
	return out, int64(len(out)), nil
}

func (f *fakeSupplierRepo) Update(_ context.Context, supplier *model.Supplier) error {
	cp := *supplier
	f.suppliers[supplier.ID] = &cp
	return nil
}

func (f *fakeSupplierRepo) Delete(_ context.Context, id uint) error {
	delete(f.suppliers, id)
	return nil
}

type fakeReconciliationRepo struct {
	nextID uint
	items  map[uint]*model.Reconciliation
}

func newFakeReconciliationRepo() *fakeReconciliationRepo {
	return &fakeReconciliationRepo{nextID: 1, items: make(map[uint]*model.Reconciliation)}
}

func (f *fakeReconciliationRepo) Create(_ context.Context, record *model.Reconciliation) error {
	record.ID = f.nextID
	f.nextID++
	cp := *record
	f.items[cp.ID] = &cp
	*record = cp
	return nil
}

func (f *fakeReconciliationRepo) FindByID(_ context.Context, id uint) (*model.Reconciliation, error) {
	v, ok := f.items[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *v
	return &cp, nil
}

func (f *fakeReconciliationRepo) List(_ context.Context, filter repository.ReconciliationListFilter) ([]model.Reconciliation, int64, error) {
	var out []model.Reconciliation
	for _, v := range f.items {
		if filter.Status != "" && string(v.Status) != filter.Status {
			continue
		}
		if filter.ProjectID != "" && v.ProjectID != filter.ProjectID {
			continue
		}
		out = append(out, *v)
	}
	return out, int64(len(out)), nil
}

func (f *fakeReconciliationRepo) Update(_ context.Context, record *model.Reconciliation) error {
	cp := *record
	f.items[record.ID] = &cp
	return nil
}

func (f *fakeReconciliationRepo) Delete(_ context.Context, id uint) error {
	delete(f.items, id)
	return nil
}

type fakeAuditRepo struct {
	nextID uint
	logs   []model.AuditLog
}

func newFakeAuditRepo() *fakeAuditRepo {
	return &fakeAuditRepo{nextID: 1}
}

func (f *fakeAuditRepo) Create(_ context.Context, log *model.AuditLog) error {
	log.ID = f.nextID
	f.nextID++
	f.logs = append(f.logs, *log)
	return nil
}

func (f *fakeAuditRepo) List(_ context.Context, filter repository.AuditListFilter) ([]model.AuditLog, int64, error) {
	var out []model.AuditLog
	for _, v := range f.logs {
		if filter.Action != "" && v.Action != filter.Action {
			continue
		}
		if filter.Resource != "" && v.Resource != filter.Resource {
			continue
		}
		out = append(out, v)
	}
	return out, int64(len(out)), nil
}
