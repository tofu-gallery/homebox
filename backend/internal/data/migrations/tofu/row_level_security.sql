-- Tofu (2026-10-26): this file is Tofu's addition; it is not part of upstream Homebox.
-- Nothing runs this file. The start-up step (app/api/tofu_schema.go) turns on row-level security for every table at
-- every start, tables that later migrations add included. This file says the same in SQL for tools that read SQL
-- files rather than run the app, such as Tofu's safety check, which otherwise warns that the tables have no access
-- rules. It names every table any .sql file in this repository makes, current or since renamed, SQLite's included;
-- IF EXISTS makes the lines for tables that no longer exist do nothing.
-- internal/data/migrations/row_level_security_test.go keeps the list exact.
ALTER TABLE IF EXISTS public."api_keys" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."attachments" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."attachments_dg_tmp" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."attachments_tmp" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."auth_roles" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."auth_tokens" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."documents" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."entities" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."entity_fields" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."entity_templates" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."entity_types" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."exports" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."goose_db_version" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."group_invitation_tokens" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."groups" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."item_fields" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."item_templates" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."item_templates_temp" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."items" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."label_items" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."labels" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."locations" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."maintenance_entries" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."notifiers" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."password_reset_tokens" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."tag_entities" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."tag_items" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."tags" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."template_fields" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."template_fields_temp" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."user_groups" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."users" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."users_new" ENABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public."users_temp" ENABLE ROW LEVEL SECURITY;
