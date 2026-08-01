-- web_exec: the work kind for the web_fetch/web_search built-in tools
-- (docs/plan/15_web-tools.md). Executed by the platform executor on BOTH
-- environment kinds — the queue's Claim admits it for self_hosted sessions
-- too, and Poll never serves it — so the kind list is the only schema change.
ALTER TABLE work_items DROP CONSTRAINT work_items_kind_check;
ALTER TABLE work_items ADD CONSTRAINT work_items_kind_check
    CHECK (kind IN ('model_turn', 'tool_exec', 'web_exec'));
