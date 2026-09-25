package repository

import (
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/renovation/renovation-budget-api/internal/model"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared&_busy_timeout=5000"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.BudgetSheet{},
		&model.BudgetItem{},
		&model.ExpenseRecord{},
		&model.Supplier{},
		&model.Reconciliation{},
		&model.Role{},
		&model.User{},
		&model.AuditLog{},
	); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}

// newFileTestDB 创建文件型 SQLite（WAL + 忙等待），用于并发写入测试；
// 测试结束自动清理并允许连接池持有多个连接。
func newFileTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + t.TempDir() + "/concurrency.db?_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite file db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(
		&model.BudgetSheet{},
		&model.BudgetItem{},
		&model.ExpenseRecord{},
	); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}
