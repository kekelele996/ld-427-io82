package service

import (
	"context"
	"sync"

	"github.com/renovation/renovation-budget-api/internal/constants"
	"github.com/renovation/renovation-budget-api/internal/model"
	"github.com/renovation/renovation-budget-api/internal/repository"
)

type fakeBudgetRepo struct {
	mu     sync.Mutex
	nextID uint
	items  map[uint]*model.BudgetSheet
}

func newFakeBudgetRepo() *fakeBudgetRepo {
	return &fakeBudgetRepo{nextID: 1, items: make(map[uint]*model.BudgetSheet)}
}

func (f *fakeBudgetRepo) Create(_ context.Context, sheet *model.BudgetSheet) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	sheet.ID = f.nextID
	f.nextID++
	cp := *sheet
	f.items[cp.ID] = &cp
	*sheet = cp
	return nil
}

func (f *fakeBudgetRepo) FindByID(_ context.Context, id uint) (*model.BudgetSheet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.items[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *v
	return &cp, nil
}

func (f *fakeBudgetRepo) List(_ context.Context, filter repository.BudgetListFilter) ([]model.BudgetSheet, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *sheet
	f.items[sheet.ID] = &cp
	return nil
}

func (f *fakeBudgetRepo) UpdateBasics(_ context.Context, sheet *model.BudgetSheet) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.items[sheet.ID]
	if !ok {
		return repository.ErrNotFound
	}
	cur.ProjectID = sheet.ProjectID
	cur.Name = sheet.Name
	cur.TotalAmount = sheet.TotalAmount
	cur.Status = sheet.Status
	cur.Version = sheet.Version
	cur.AvailableAmount = CalculateAvailable(cur.TotalAmount, cur.SpentAmount, cur.FrozenAmount)
	return nil
}

func (f *fakeBudgetRepo) Delete(_ context.Context, id uint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, id)
	return nil
}

func (f *fakeBudgetRepo) AdjustAmounts(_ context.Context, id uint, patch repository.BudgetAmountPatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	sheet, ok := f.items[id]
	if !ok {
		return repository.ErrNotFound
	}
	if sheet.SpentAmount+patch.SpentDelta < 0 || sheet.FrozenAmount+patch.FrozenDelta < 0 {
		return repository.ErrInvalidState
	}
	sheet.SpentAmount += patch.SpentDelta
	sheet.FrozenAmount += patch.FrozenDelta
	sheet.AvailableAmount = CalculateAvailable(sheet.TotalAmount, sheet.SpentAmount, sheet.FrozenAmount)
	return nil
}

type fakeItemRepo struct {
	mu     sync.Mutex
	nextID uint
	items  map[uint]*model.BudgetItem
}

func newFakeItemRepo() *fakeItemRepo {
	return &fakeItemRepo{nextID: 1, items: make(map[uint]*model.BudgetItem)}
}

func (f *fakeItemRepo) Create(_ context.Context, item *model.BudgetItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	item.ID = f.nextID
	f.nextID++
	cp := *item
	f.items[cp.ID] = &cp
	*item = cp
	return nil
}

func (f *fakeItemRepo) FindByID(_ context.Context, id uint) (*model.BudgetItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.items[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *v
	return &cp, nil
}

func (f *fakeItemRepo) ListByBudgetID(_ context.Context, budgetSheetID uint) ([]model.BudgetItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.BudgetItem
	for _, v := range f.items {
		if v.BudgetSheetID == budgetSheetID {
			out = append(out, *v)
		}
	}
	return out, nil
}

func (f *fakeItemRepo) Update(_ context.Context, item *model.BudgetItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *item
	f.items[item.ID] = &cp
	return nil
}

func (f *fakeItemRepo) UpdateBasics(_ context.Context, item *model.BudgetItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.items[item.ID]
	if !ok {
		return repository.ErrNotFound
	}
	if item.BudgetAmount < cur.SpentAmount+cur.FrozenAmount {
		return repository.ErrBudgetConflict
	}
	cur.Category = item.Category
	cur.SubCategory = item.SubCategory
	cur.BudgetAmount = item.BudgetAmount
	cur.SortOrder = item.SortOrder
	cur.Remark = item.Remark
	cur.VarianceAmount = CalculateVariance(cur.SpentAmount, cur.BudgetAmount)
	cur.AvailableAmount = CalculateItemAvailable(cur.BudgetAmount, cur.SpentAmount, cur.FrozenAmount)
	return nil
}

func (f *fakeItemRepo) Delete(_ context.Context, id uint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, id)
	return nil
}

// ReserveFrozen 模拟数据库条件 UPDATE：校验与占用在同一把锁内原子完成。
func (f *fakeItemRepo) ReserveFrozen(_ context.Context, itemID uint, amount float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.items[itemID]
	if !ok {
		return repository.ErrNotFound
	}
	if CalculateItemAvailable(item.BudgetAmount, item.SpentAmount, item.FrozenAmount) < amount {
		return repository.ErrBudgetConflict
	}
	item.FrozenAmount += amount
	item.AvailableAmount = CalculateItemAvailable(item.BudgetAmount, item.SpentAmount, item.FrozenAmount)
	return nil
}

func (f *fakeItemRepo) ConfirmFrozen(_ context.Context, itemID uint, amount float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.items[itemID]
	if !ok {
		return repository.ErrNotFound
	}
	if item.FrozenAmount < amount {
		return repository.ErrInvalidState
	}
	item.FrozenAmount -= amount
	item.SpentAmount += amount
	item.AvailableAmount = CalculateItemAvailable(item.BudgetAmount, item.SpentAmount, item.FrozenAmount)
	item.VarianceAmount = CalculateVariance(item.SpentAmount, item.BudgetAmount)
	return nil
}

func (f *fakeItemRepo) ReleaseFrozen(_ context.Context, itemID uint, amount float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.items[itemID]
	if !ok {
		return repository.ErrNotFound
	}
	if item.FrozenAmount < amount {
		return repository.ErrInvalidState
	}
	item.FrozenAmount -= amount
	item.AvailableAmount = CalculateItemAvailable(item.BudgetAmount, item.SpentAmount, item.FrozenAmount)
	return nil
}

type fakeExpenseRepo struct {
	mu      sync.Mutex
	nextID  uint
	records map[uint]*model.ExpenseRecord
}

func newFakeExpenseRepo() *fakeExpenseRepo {
	return &fakeExpenseRepo{nextID: 1, records: make(map[uint]*model.ExpenseRecord)}
}

func (f *fakeExpenseRepo) Create(_ context.Context, record *model.ExpenseRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	record.ID = f.nextID
	f.nextID++
	cp := *record
	f.records[cp.ID] = &cp
	*record = cp
	return nil
}

func (f *fakeExpenseRepo) FindByID(_ context.Context, id uint) (*model.ExpenseRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.records[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	cp := *v
	return &cp, nil
}

func (f *fakeExpenseRepo) List(_ context.Context, filter repository.ExpenseListFilter) ([]model.ExpenseRecord, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *record
	f.records[record.ID] = &cp
	return nil
}

func (f *fakeExpenseRepo) TransitionStatus(_ context.Context, id uint, wantStatus constants.ExpenseStatus, patch repository.ExpenseStatusPatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, ok := f.records[id]
	if !ok {
		return repository.ErrNotFound
	}
	if record.Status != wantStatus {
		return repository.ErrInvalidState
	}
	record.Status = patch.Status
	if patch.ApprovedByID != nil {
		record.ApprovedByID = patch.ApprovedByID
	}
	if patch.ApprovalComment != "" {
		record.ApprovalComment = patch.ApprovalComment
	}
	if patch.PaymentDate != nil {
		record.PaymentDate = patch.PaymentDate
	}
	return nil
}

// fakeTxManager 用一把全局锁串行化事务，模拟数据库事务的原子性。
type fakeTxManager struct {
	mu sync.Mutex
}

func newFakeTxManager() *fakeTxManager {
	return &fakeTxManager{}
}

type fakeTxKey struct{}

func (m *fakeTxManager) WithinTransaction(ctx context.Context, fn func(txCtx context.Context) error) error {
	if ctx.Value(fakeTxKey{}) != nil {
		return fn(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return fn(context.WithValue(ctx, fakeTxKey{}, struct{}{}))
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
