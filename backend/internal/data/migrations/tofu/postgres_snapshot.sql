-- tofu-schema-snapshot {"migrations":[{"version":20241027025146,"file":"20241027025146_init.sql","sha256":"fcbb4c78ccc3f58d00865072044df4eda2c42c0cbb13daea6e4cee0ee09fc63a"},{"version":20250112202302,"file":"20250112202302_sync_children.go","sha256":"a3ffbc92a071591c42c2bd457b471f9e3b4d377020d87ca11c338e443efe14b4"},{"version":20250419184104,"file":"20250419184104_merge_docs_attachments.sql","sha256":"10272aa9362a20bb78a4817ed093d8276b19c7531f2c588f37f19a7c1f2d4626"},{"version":20250619215101,"file":"20250619215101_add_thumbnails.sql","sha256":"19ae29277ecc727730e5e23fdd0cc0c127a63b4a14a650dceb92407716d87078"},{"version":20250625120010,"file":"20250625120010_add_mime_type.sql","sha256":"7e7c806599a31fc1da16641c76592f4ce9dbb8b5b9d4f4970c344be0bbbe3803"},{"version":20250629112901,"file":"20250629112901_fix_thumbnail_foriegn.sql","sha256":"4461a32cf99fd87f0acf9c830b431ab2f5a016561340a052f65f4570d0115ccc"},{"version":20250826000000,"file":"20250826000000_make_attachment_paths_relative.sql","sha256":"7ae9e9679d3fb793dd7b9a56299cdd0a91aa693fecb285fe73ca832a81a84a34"},{"version":20251123000000,"file":"20251123000000_add_oidc_identity.sql","sha256":"97c5d15b8b5b82ae3d6ca89ca174455174f4e03cc17316960d3b68a4d0ba6c39"},{"version":20251128120201,"file":"20251128120201_recreate_item_templates.sql","sha256":"67ba3b81344d6976d9eb3e99a5f44bf154fe9f6b3bf5f68ad92d007c6ee224a7"},{"version":20251129000000,"file":"20251129000000_make_password_nullable.sql","sha256":"62e1063777ab42501eb9c30dc237dd39ad3700c79627203346c05fd26662f522"},{"version":20251226000000,"file":"20251226000000_user_group_m2m.sql","sha256":"3501381943273d6fd8dd9ba8d89892a8d9deade585c7e81e1522ddb10c8afecf"},{"version":20251228214900,"file":"20251228214900_rename_labels_to_tags.sql","sha256":"f57140d8246c9539699d89fc36cb5fb5e8ce229ee2f5c2033f5ca491a317298b"},{"version":20260211200601,"file":"20260211200601_template_custom_fields_types.sql","sha256":"0b68a804f70ec33514eb1c4387e3d75ec85b3c7bd059e6efdfffbc2ffe0d780d"},{"version":20260215000000,"file":"20260215000000_add_tag_children.sql","sha256":"1395e4010508796a67f4031a18ad186237adae022e5dbbb2e2ee7761825f0052"},{"version":20260314103001,"file":"20260314103001_item_quantity_decimals.sql","sha256":"f58c8ebbd83af2ec9bacff8647ecb3675b9d51fc605546625bec8c7621a3b2e4"},{"version":20260322121100,"file":"20260322121100_user_settings.sql","sha256":"6f56167f2cd87deda395baffb468ec700e656e2eef605b03b64f7549434fe620"},{"version":20260416120000,"file":"20260416120000_merge_entities.go","sha256":"7736490bd0f3ef24fc63e5e002b2819a2057811d4c574d3f48fb614d808316e5"},{"version":20260417120001,"file":"20260417120001_entity_type_default_template.sql","sha256":"1c6401cc747f5cae8e2ba419e282d9ed30c5db77c637adffe01feced0ac25f4c"},{"version":20260425000000,"file":"20260425000000_rename_date_columns.sql","sha256":"e5a55408de23765756fedc3723e7df5838c48951ca8bdbd5d1aeac4e8093dbb8"},{"version":20260510000000,"file":"20260510000000_add_api_keys.sql","sha256":"11e3f436ab572a03c2c4daa9a4d4421678b7450664e8d6cdf2382ed9e65035b5"},{"version":20260511000000,"file":"20260511000000_per_group_role.sql","sha256":"2efc1cb8d6065da3ef60e5911993d193b43556663a7692c497e376aaf94c8569"},{"version":20260512000001,"file":"20260512000001_add_password_reset_tokens.sql","sha256":"e739a0ac3156075a31cde37a513414bcea72057bc81f29b24c53a48ead1e8f45"},{"version":20260512130001,"file":"20260512130001_add_exports.sql","sha256":"0ce106fbcb0a98b69ed1f6831127bf21c11244b31e77730e97b0b80a35866c2f"},{"version":20260821000000,"file":"20260821000000_entity_location_override.sql","sha256":"92d949bcd76674969fec09e5b546a65feef30584bab5884c7e572ce034b92ff6"}],"tables":["api_keys","attachments","auth_roles","auth_tokens","entities","entity_fields","entity_templates","entity_types","exports","goose_db_version","group_invitation_tokens","groups","maintenance_entries","notifiers","password_reset_tokens","tag_entities","tags","template_fields","user_groups","users"]}
-- Tofu (2026-10-26): made by app/tools/tofusnapshot from a database that goose's postgres migrations brought up to date. Do not edit; run the tool again.
CREATE TABLE public.api_keys (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    user_id uuid NOT NULL,
    name character varying NOT NULL,
    token bytea NOT NULL,
    expires_at timestamp with time zone,
    last_used_at timestamp with time zone
);
CREATE TABLE public.attachments (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    type character varying DEFAULT 'attachment'::character varying NOT NULL,
    "primary" boolean DEFAULT false NOT NULL,
    entity_attachments uuid,
    title character varying DEFAULT ''::character varying NOT NULL,
    path character varying DEFAULT ''::character varying NOT NULL,
    attachment_thumbnail uuid,
    mime_type character varying DEFAULT 'application/octet-stream'::character varying,
    CONSTRAINT attachments_no_self_reference CHECK ((id <> attachment_thumbnail))
);
CREATE TABLE public.auth_roles (
    id bigint NOT NULL,
    role character varying DEFAULT 'user'::character varying NOT NULL,
    auth_tokens_roles uuid
);
ALTER TABLE public.auth_roles ALTER COLUMN id ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME public.auth_roles_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);
CREATE TABLE public.auth_tokens (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    token bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    user_auth_tokens uuid
);
CREATE TABLE public.entities (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    name character varying NOT NULL,
    description character varying,
    import_ref character varying,
    notes character varying,
    quantity double precision DEFAULT 1 NOT NULL,
    insured boolean DEFAULT false NOT NULL,
    archived boolean DEFAULT false NOT NULL,
    asset_id bigint DEFAULT 0 NOT NULL,
    serial_number character varying,
    model_number character varying,
    manufacturer character varying,
    lifetime_warranty boolean DEFAULT false NOT NULL,
    warranty_expires timestamp with time zone,
    warranty_details character varying,
    purchase_date timestamp with time zone,
    purchase_from character varying,
    purchase_price double precision DEFAULT 0 NOT NULL,
    sold_date timestamp with time zone,
    sold_to character varying,
    sold_price double precision DEFAULT 0 NOT NULL,
    sold_notes character varying,
    group_entities uuid NOT NULL,
    entity_children uuid,
    sync_child_entity_locations boolean DEFAULT false NOT NULL,
    entity_type_entities uuid NOT NULL,
    entity_location_entities uuid
);
CREATE TABLE public.entity_fields (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    name character varying NOT NULL,
    description character varying,
    type character varying NOT NULL,
    text_value character varying,
    number_value bigint,
    boolean_value boolean DEFAULT false NOT NULL,
    time_value timestamp with time zone NOT NULL,
    entity_fields uuid
);
CREATE TABLE public.entity_templates (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    name character varying NOT NULL,
    description character varying,
    notes character varying,
    default_quantity double precision DEFAULT 1 NOT NULL,
    default_insured boolean DEFAULT false NOT NULL,
    default_name character varying,
    default_description character varying,
    default_manufacturer character varying,
    default_model_number character varying,
    default_lifetime_warranty boolean DEFAULT false NOT NULL,
    default_warranty_details character varying,
    include_warranty_fields boolean DEFAULT false NOT NULL,
    include_purchase_fields boolean DEFAULT false NOT NULL,
    include_sold_fields boolean DEFAULT false NOT NULL,
    default_tag_ids jsonb,
    entity_template_location uuid,
    group_entity_templates uuid NOT NULL
);
CREATE TABLE public.entity_types (
    id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    name character varying(255) NOT NULL,
    description character varying(1000),
    is_location boolean DEFAULT false NOT NULL,
    icon character varying(255),
    group_entity_types uuid NOT NULL,
    entity_type_default_template uuid
);
CREATE TABLE public.exports (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    kind character varying DEFAULT 'export'::character varying NOT NULL,
    status character varying DEFAULT 'pending'::character varying NOT NULL,
    progress bigint DEFAULT 0 NOT NULL,
    artifact_path character varying,
    size_bytes bigint DEFAULT 0 NOT NULL,
    error character varying,
    group_id uuid NOT NULL,
    CONSTRAINT exports_error_check CHECK (((error IS NULL) OR (char_length((error)::text) <= 1000))),
    CONSTRAINT exports_kind_check CHECK (((kind)::text = ANY ((ARRAY['export'::character varying, 'import'::character varying])::text[]))),
    CONSTRAINT exports_status_check CHECK (((status)::text = ANY ((ARRAY['pending'::character varying, 'running'::character varying, 'completed'::character varying, 'failed'::character varying])::text[])))
);
CREATE TABLE public.goose_db_version (
    id integer NOT NULL,
    version_id bigint NOT NULL,
    is_applied boolean NOT NULL,
    tstamp timestamp without time zone DEFAULT now() NOT NULL
);
ALTER TABLE public.goose_db_version ALTER COLUMN id ADD GENERATED BY DEFAULT AS IDENTITY (
    SEQUENCE NAME public.goose_db_version_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);
CREATE TABLE public.group_invitation_tokens (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    token bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    uses bigint DEFAULT 0 NOT NULL,
    group_invitation_tokens uuid
);
CREATE TABLE public.groups (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    name character varying NOT NULL,
    currency character varying DEFAULT 'usd'::character varying NOT NULL
);
CREATE TABLE public.maintenance_entries (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    date timestamp with time zone,
    scheduled_date timestamp with time zone,
    name character varying NOT NULL,
    description character varying,
    cost double precision DEFAULT 0 NOT NULL,
    entity_id uuid NOT NULL
);
CREATE TABLE public.notifiers (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    name character varying NOT NULL,
    url character varying NOT NULL,
    is_active boolean DEFAULT true NOT NULL,
    group_id uuid NOT NULL,
    user_id uuid NOT NULL
);
CREATE TABLE public.password_reset_tokens (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    user_id uuid NOT NULL,
    token bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    used_at timestamp with time zone
);
CREATE TABLE public.tag_entities (
    tag_id uuid NOT NULL,
    entity_id uuid NOT NULL
);
CREATE TABLE public.tags (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    name character varying NOT NULL,
    description character varying,
    color character varying,
    group_tags uuid NOT NULL,
    icon character varying(255),
    tag_children uuid
);
CREATE TABLE public.template_fields (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    name character varying NOT NULL,
    description character varying,
    type character varying NOT NULL,
    text_value character varying,
    entity_template_fields uuid,
    number_value integer,
    boolean_value boolean DEFAULT false,
    time_value timestamp without time zone
);
CREATE TABLE public.user_groups (
    user_id uuid NOT NULL,
    group_id uuid NOT NULL,
    role character varying DEFAULT 'user'::character varying NOT NULL
);
CREATE TABLE public.users (
    id uuid NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    name character varying NOT NULL,
    email character varying NOT NULL,
    password character varying,
    is_superuser boolean DEFAULT false NOT NULL,
    superuser boolean DEFAULT false NOT NULL,
    activated_on timestamp with time zone,
    oidc_issuer character varying,
    oidc_subject character varying,
    default_group_id uuid,
    settings jsonb DEFAULT '{}'::jsonb
);
SELECT pg_catalog.setval('public.auth_roles_id_seq', 1, false);
ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.attachments
    ADD CONSTRAINT attachments_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.auth_roles
    ADD CONSTRAINT auth_roles_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.auth_tokens
    ADD CONSTRAINT auth_tokens_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.entity_types
    ADD CONSTRAINT entity_types_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.exports
    ADD CONSTRAINT exports_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.goose_db_version
    ADD CONSTRAINT goose_db_version_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.group_invitation_tokens
    ADD CONSTRAINT group_invitation_tokens_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.groups
    ADD CONSTRAINT groups_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.entity_fields
    ADD CONSTRAINT item_fields_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.entity_templates
    ADD CONSTRAINT item_templates_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.entities
    ADD CONSTRAINT items_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.tag_entities
    ADD CONSTRAINT label_items_pkey PRIMARY KEY (tag_id, entity_id);
ALTER TABLE ONLY public.tags
    ADD CONSTRAINT labels_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.maintenance_entries
    ADD CONSTRAINT maintenance_entries_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.notifiers
    ADD CONSTRAINT notifiers_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.password_reset_tokens
    ADD CONSTRAINT password_reset_tokens_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.template_fields
    ADD CONSTRAINT template_fields_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.user_groups
    ADD CONSTRAINT user_groups_pkey PRIMARY KEY (user_id, group_id);
ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);
CREATE UNIQUE INDEX api_keys_token_key ON public.api_keys USING btree (token);
CREATE INDEX apikey_token ON public.api_keys USING btree (token);
CREATE INDEX apikey_user_id ON public.api_keys USING btree (user_id);
CREATE UNIQUE INDEX auth_roles_auth_tokens_roles_key ON public.auth_roles USING btree (auth_tokens_roles);
CREATE UNIQUE INDEX auth_tokens_token_key ON public.auth_tokens USING btree (token);
CREATE INDEX export_group_id ON public.exports USING btree (group_id);
CREATE INDEX export_group_id_status ON public.exports USING btree (group_id, status);
CREATE UNIQUE INDEX group_invitation_tokens_token_key ON public.group_invitation_tokens USING btree (token);
CREATE INDEX idx_entities_location ON public.entities USING btree (entity_location_entities);
CREATE INDEX item_archived ON public.entities USING btree (archived);
CREATE INDEX item_asset_id ON public.entities USING btree (asset_id);
CREATE INDEX item_manufacturer ON public.entities USING btree (manufacturer);
CREATE INDEX item_model_number ON public.entities USING btree (model_number);
CREATE INDEX item_name ON public.entities USING btree (name);
CREATE INDEX item_serial_number ON public.entities USING btree (serial_number);
CREATE INDEX notifier_group_id ON public.notifiers USING btree (group_id);
CREATE INDEX notifier_group_id_is_active ON public.notifiers USING btree (group_id, is_active);
CREATE INDEX notifier_user_id ON public.notifiers USING btree (user_id);
CREATE INDEX notifier_user_id_is_active ON public.notifiers USING btree (user_id, is_active);
CREATE UNIQUE INDEX password_reset_tokens_token_key ON public.password_reset_tokens USING btree (token);
CREATE INDEX passwordresettokens_user_id ON public.password_reset_tokens USING btree (user_id);
CREATE UNIQUE INDEX users_email_key ON public.users USING btree (email);
CREATE UNIQUE INDEX users_oidc_issuer_subject_key ON public.users USING btree (oidc_issuer, oidc_subject) WHERE ((oidc_issuer IS NOT NULL) AND (oidc_subject IS NOT NULL));
ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_users_api_keys FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.attachments
    ADD CONSTRAINT attachments_attachments_thumbnail FOREIGN KEY (attachment_thumbnail) REFERENCES public.attachments(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.attachments
    ADD CONSTRAINT attachments_items_attachments FOREIGN KEY (entity_attachments) REFERENCES public.entities(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.auth_roles
    ADD CONSTRAINT auth_roles_auth_tokens_roles FOREIGN KEY (auth_tokens_roles) REFERENCES public.auth_tokens(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.auth_tokens
    ADD CONSTRAINT auth_tokens_users_auth_tokens FOREIGN KEY (user_auth_tokens) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.entities
    ADD CONSTRAINT entities_entities_location_entities FOREIGN KEY (entity_location_entities) REFERENCES public.entities(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.entities
    ADD CONSTRAINT entities_entity_types_entities FOREIGN KEY (entity_type_entities) REFERENCES public.entity_types(id) ON DELETE RESTRICT;
ALTER TABLE ONLY public.entity_templates
    ADD CONSTRAINT entity_templates_entities_location FOREIGN KEY (entity_template_location) REFERENCES public.entities(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.entity_types
    ADD CONSTRAINT entity_types_entity_templates_default_template FOREIGN KEY (entity_type_default_template) REFERENCES public.entity_templates(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.entity_types
    ADD CONSTRAINT entity_types_groups_entity_types FOREIGN KEY (group_entity_types) REFERENCES public.groups(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.exports
    ADD CONSTRAINT exports_groups_exports FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.group_invitation_tokens
    ADD CONSTRAINT group_invitation_tokens_groups_invitation_tokens FOREIGN KEY (group_invitation_tokens) REFERENCES public.groups(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.entity_fields
    ADD CONSTRAINT item_fields_items_fields FOREIGN KEY (entity_fields) REFERENCES public.entities(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.entity_templates
    ADD CONSTRAINT item_templates_groups_item_templates FOREIGN KEY (group_entity_templates) REFERENCES public.groups(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.entities
    ADD CONSTRAINT items_groups_items FOREIGN KEY (group_entities) REFERENCES public.groups(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.entities
    ADD CONSTRAINT items_items_children FOREIGN KEY (entity_children) REFERENCES public.entities(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.tag_entities
    ADD CONSTRAINT label_items_item_id FOREIGN KEY (entity_id) REFERENCES public.entities(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.tag_entities
    ADD CONSTRAINT label_items_label_id FOREIGN KEY (tag_id) REFERENCES public.tags(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.tags
    ADD CONSTRAINT labels_groups_labels FOREIGN KEY (group_tags) REFERENCES public.groups(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.maintenance_entries
    ADD CONSTRAINT maintenance_entries_items_maintenance_entries FOREIGN KEY (entity_id) REFERENCES public.entities(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.notifiers
    ADD CONSTRAINT notifiers_groups_notifiers FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.notifiers
    ADD CONSTRAINT notifiers_users_notifiers FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.password_reset_tokens
    ADD CONSTRAINT password_reset_tokens_users_password_reset_tokens FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.tags
    ADD CONSTRAINT tags_tag_children_fkey FOREIGN KEY (tag_children) REFERENCES public.tags(id) ON DELETE SET NULL;
ALTER TABLE ONLY public.template_fields
    ADD CONSTRAINT template_fields_item_templates_fields FOREIGN KEY (entity_template_fields) REFERENCES public.entity_templates(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.user_groups
    ADD CONSTRAINT user_groups_group_id FOREIGN KEY (group_id) REFERENCES public.groups(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.user_groups
    ADD CONSTRAINT user_groups_user_id FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_groups_users_default FOREIGN KEY (default_group_id) REFERENCES public.groups(id) ON DELETE SET NULL;
INSERT INTO public.goose_db_version (version_id, is_applied) VALUES
	(0, true),
	(20241027025146, true),
	(20250112202302, true),
	(20250419184104, true),
	(20250619215101, true),
	(20250625120010, true),
	(20250629112901, true),
	(20250826000000, true),
	(20251123000000, true),
	(20251128120201, true),
	(20251129000000, true),
	(20251226000000, true),
	(20251228214900, true),
	(20260211200601, true),
	(20260215000000, true),
	(20260314103001, true),
	(20260322121100, true),
	(20260416120000, true),
	(20260417120001, true),
	(20260425000000, true),
	(20260510000000, true),
	(20260511000000, true),
	(20260512000001, true),
	(20260512130001, true),
	(20260821000000, true);
ALTER TABLE public."api_keys" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."attachments" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."auth_roles" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."auth_tokens" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."entities" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."entity_fields" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."entity_templates" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."entity_types" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."exports" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."goose_db_version" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."group_invitation_tokens" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."groups" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."maintenance_entries" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."notifiers" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."password_reset_tokens" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."tag_entities" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."tags" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."template_fields" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."user_groups" ENABLE ROW LEVEL SECURITY;
ALTER TABLE public."users" ENABLE ROW LEVEL SECURITY;
