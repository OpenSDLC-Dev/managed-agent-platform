-- Deploy only after the native-only slice 7a binary is accepted. That binary
-- neither reads these tables nor writes per-key budgets. A rollback to an older
-- binary requires restoring the database and its deployment configuration.
-- Keep API key rows, usage history, organization model budgets and initiators.
DROP TABLE modelgateway.key_policies;
DROP FUNCTION modelgateway.key_policy_require_unowned();
DROP TABLE modelgateway.rate_windows;
