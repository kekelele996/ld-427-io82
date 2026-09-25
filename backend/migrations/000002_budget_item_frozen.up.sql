-- 预算项额度控制：新增占用（审批中）金额与可用额度。
ALTER TABLE budget_items
    ADD COLUMN IF NOT EXISTS frozen_amount DOUBLE PRECISION NOT NULL DEFAULT 0;

ALTER TABLE budget_items
    ADD COLUMN IF NOT EXISTS available_amount DOUBLE PRECISION NOT NULL DEFAULT 0;

-- 可用额度 = 预算金额 - 已支出 - 审批中占用。
UPDATE budget_items
SET available_amount = budget_amount - spent_amount - frozen_amount
WHERE available_amount = 0;
