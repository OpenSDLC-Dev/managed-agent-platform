-- The retired control-plane seeder wrote in the default tenancy without an
-- issuer or owner. Operators can also name an issued key 'bootstrap'.
DELETE FROM api_keys
 WHERE org_id = 'default' AND workspace_id = 'default' AND project_id = 'default'
   AND name = 'bootstrap' AND created_by IS NULL AND owner_principal_id IS NULL;
