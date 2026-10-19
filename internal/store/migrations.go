package store

// migration is one versioned schema step.
//
// The checksum is computed over sql at first apply and stored in
// schema_migrations. RULES (learned the hard way in the upstream project this
// design borrows from):
//
//  1. NEVER edit the sql of a migration that has shipped. Existing databases
//     store the original checksum and will refuse to boot with a mismatch.
//     To change schema, append a new migration.
//  2. Because the checksum covers the raw string, even reindenting the SQL
//     counts as an edit. Tabs vs spaces matter.
type migration struct {
	filename string
	sql      string
}

// migrations is the ordered schema history. Append only.
var migrations = []migration{
	{
		filename: "001_initial_schema.sql",
		sql: `
CREATE TABLE IF NOT EXISTS accounts (
  id                TEXT PRIMARY KEY,
  name              TEXT NOT NULL,
  remote_uid        TEXT NOT NULL DEFAULT '',
  provider          TEXT NOT NULL DEFAULT 'workbuddy',
  provider_region   TEXT NOT NULL DEFAULT 'cn',
  auth_type         TEXT NOT NULL DEFAULT 'none',
  enabled           INTEGER NOT NULL DEFAULT 1,
  max_inflight      INTEGER NOT NULL DEFAULT 4,
  priority          INTEGER NOT NULL DEFAULT 50,
  weight            INTEGER NOT NULL DEFAULT 50,
  status            TEXT NOT NULL DEFAULT 'offline',
  last_error        TEXT NOT NULL DEFAULT '',
  last_error_kind   TEXT NOT NULL DEFAULT '',
  cooldown_until    TEXT,
  drop_system_prompt INTEGER NOT NULL DEFAULT 1,
  auto_checkin      INTEGER NOT NULL DEFAULT 0,
  last_checkin_at   TEXT NOT NULL DEFAULT '',
  last_checkin_status TEXT NOT NULL DEFAULT '',
  last_checkin_msg  TEXT NOT NULL DEFAULT '',
  created_at        TEXT NOT NULL,
  updated_at        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS accounts_provider_region ON accounts(provider, provider_region);

CREATE TABLE IF NOT EXISTS account_credentials (
  account_id  TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  format      TEXT NOT NULL,
  payload     BLOB NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  key_hash    TEXT NOT NULL UNIQUE,
  key_prefix  TEXT NOT NULL DEFAULT '',
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  TEXT NOT NULL,
  last_used_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS app_secrets (
  name        TEXT PRIMARY KEY,
  value       TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS request_logs (
  id                 TEXT PRIMARY KEY,
  created_at         TEXT NOT NULL,
  finished_at        TEXT,
  stream             INTEGER NOT NULL DEFAULT 0,
  status             TEXT NOT NULL,
  protocol           TEXT NOT NULL DEFAULT 'chat.completions',
  requested_model    TEXT NOT NULL DEFAULT '',
  mapped_model       TEXT NOT NULL DEFAULT '',
  account_id         TEXT,
  provider           TEXT NOT NULL DEFAULT '',
  routing            TEXT NOT NULL DEFAULT '',
  prompt_tokens      INTEGER,
  completion_tokens  INTEGER,
  total_tokens       INTEGER,
  credits            REAL,
  latency_ms         INTEGER,
  ttfb_ms            INTEGER,
  error_kind         TEXT NOT NULL DEFAULT '',
  error_code         TEXT NOT NULL DEFAULT '',
  error_message      TEXT NOT NULL DEFAULT '',
  attempt_count      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS request_logs_created_at ON request_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS request_logs_account_id ON request_logs(account_id);
CREATE INDEX IF NOT EXISTS request_logs_status ON request_logs(status);
CREATE INDEX IF NOT EXISTS request_logs_provider ON request_logs(provider);

CREATE TABLE IF NOT EXISTS request_attempts (
  id             TEXT PRIMARY KEY,
  request_id     TEXT NOT NULL REFERENCES request_logs(id) ON DELETE CASCADE,
  attempt_index  INTEGER NOT NULL,
  account_id     TEXT NOT NULL DEFAULT '',
  provider       TEXT NOT NULL DEFAULT '',
  status         TEXT NOT NULL DEFAULT '',
  error_kind     TEXT NOT NULL DEFAULT '',
  error_code     TEXT NOT NULL DEFAULT '',
  error_message  TEXT NOT NULL DEFAULT '',
  latency_ms     INTEGER,
  started_at     TEXT NOT NULL DEFAULT '',
  finished_at    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS request_attempts_request_id ON request_attempts(request_id);

CREATE TABLE IF NOT EXISTS checkin_records (
  id          TEXT PRIMARY KEY,
  account_id  TEXT NOT NULL,
  status      TEXT NOT NULL,
  message     TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS checkin_records_account ON checkin_records(account_id, created_at DESC);`,
	},
	{
		filename: "002_credential_metadata.sql",
		sql: `
CREATE TABLE IF NOT EXISTS credential_metadata (
  account_id  TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  nickname    TEXT NOT NULL DEFAULT '',
  domain      TEXT NOT NULL DEFAULT '',
  enterprise_id TEXT NOT NULL DEFAULT '',
  expires_at  TEXT NOT NULL DEFAULT '',
  updated_at  TEXT NOT NULL
);`,
	},
	{
		// Armor opt-in per account, plus the request-time bookkeeping needed to
		// report what was sent.
		//
		// This is a NEW migration rather than an edit to 001: editing an applied
		// migration changes its checksum and every existing database refuses to
		// boot. That rule is why schema changes are append-only here.
		filename: "003_armor_and_access.sql",
		sql: `
ALTER TABLE accounts ADD COLUMN armor_preset TEXT NOT NULL DEFAULT '';
ALTER TABLE request_logs ADD COLUMN armor_preset TEXT NOT NULL DEFAULT '';`,
	},
	{
		// A tiny key/value table for settings that must survive a restart and be
		// changeable at runtime, without a schema change each time.
		//
		// The first user is the global armor switch. It is deliberately NOT
		// stored per account: armor is chosen from the requested MODEL, and a
		// single account serves many models, so an account-level switch would
		// turn the preset on for models it was never meant for.
		filename: "004_settings.sql",
		sql: `
CREATE TABLE IF NOT EXISTS settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL
);`,
	},
	{
		// 只追加迁移，保留已发布版本的校验和及旧账号配置。
		filename: "005_pool_runtime.sql",
		sql: `
ALTER TABLE accounts ADD COLUMN pool_runtime TEXT NOT NULL DEFAULT '{}';
ALTER TABLE accounts ADD COLUMN quota_scope TEXT NOT NULL DEFAULT 'shared';`,
	},
	{
		// 补齐额度组及真实额度到期时间，保持历史迁移不变。
		filename: "006_quota_metadata.sql",
		sql: `
ALTER TABLE accounts ADD COLUMN quota_group TEXT NOT NULL DEFAULT '';
ALTER TABLE accounts ADD COLUMN quota_expires_at TEXT NOT NULL DEFAULT '';`,
	},
	{
		// 只追加迁移，旧密钥保留管理权限，避免升级后无法进入后台。
		filename: "007_api_key_scope.sql",
		sql: `
ALTER TABLE api_keys ADD COLUMN scope TEXT NOT NULL DEFAULT 'admin' CHECK (scope IN ('admin','inference'));`,
	},
	{
		// 新权限独立保存，旧记录在服务初始化时继承历史破甲状态。
		filename: "008_api_key_permissions.sql",
		sql:      `ALTER TABLE api_keys ADD COLUMN permissions TEXT NOT NULL DEFAULT '';`,
	},
	{
		// 用户登录独立于管理员密钥；只追加迁移，不改动已有账号和授权。
		filename: "009_user_portal.sql",
		sql: `
CREATE TABLE portal_users (
  id TEXT PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','active','disabled')),
  api_key_id TEXT NOT NULL UNIQUE REFERENCES api_keys(id),
  daily_limit INTEGER NOT NULL DEFAULT 100 CHECK(daily_limit >= 0),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE portal_sessions (
  session_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES portal_users(id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX portal_sessions_user ON portal_sessions(user_id);
CREATE INDEX portal_sessions_expiry ON portal_sessions(expires_at);
CREATE TABLE portal_daily_usage (
  user_id TEXT NOT NULL REFERENCES portal_users(id) ON DELETE CASCADE,
  day TEXT NOT NULL,
  requests INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(user_id, day)
);
ALTER TABLE request_logs ADD COLUMN api_key_id TEXT NOT NULL DEFAULT '';
CREATE INDEX request_logs_key_created ON request_logs(api_key_id, created_at);`,
	},
	{
		// 记录历史授权归属，轮换不重新启用旧密钥，也不丢失用户的历史用量。
		filename: "010_portal_authorization_history.sql",
		sql: `
CREATE TABLE portal_user_keys (
  api_key_id TEXT PRIMARY KEY REFERENCES api_keys(id),
  user_id TEXT NOT NULL REFERENCES portal_users(id) ON DELETE CASCADE
);
CREATE INDEX portal_user_keys_user ON portal_user_keys(user_id);
INSERT INTO portal_user_keys(api_key_id,user_id) SELECT api_key_id,id FROM portal_users;`,
	},
	{
		// 资金账本独立于可清理的请求日志；金额以百万分之一元整数保存。
		filename: "011_portal_commerce.sql",
		sql: `
CREATE TABLE portal_wallets(user_id TEXT PRIMARY KEY REFERENCES portal_users(id),balance INTEGER NOT NULL DEFAULT 0 CHECK(balance>=0),held INTEGER NOT NULL DEFAULT 0 CHECK(held>=0 AND held<=balance));
CREATE TABLE model_prices(model TEXT PRIMARY KEY,input_rate INTEGER NOT NULL CHECK(input_rate>=0),output_rate INTEGER NOT NULL CHECK(output_rate>=0),enabled INTEGER NOT NULL DEFAULT 1);
CREATE TABLE portal_plans(id TEXT PRIMARY KEY,name TEXT NOT NULL,price INTEGER NOT NULL CHECK(price>0),allowance INTEGER NOT NULL CHECK(allowance>0),days INTEGER NOT NULL CHECK(days>0),enabled INTEGER NOT NULL DEFAULT 1);
CREATE TABLE portal_subscriptions(id TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES portal_users(id),plan_id TEXT NOT NULL,name TEXT NOT NULL,allowance INTEGER NOT NULL,used INTEGER NOT NULL DEFAULT 0,held INTEGER NOT NULL DEFAULT 0,starts_at TEXT NOT NULL,ends_at TEXT NOT NULL,CHECK(used>=0 AND held>=0 AND used+held<=allowance));
CREATE INDEX portal_subscriptions_user ON portal_subscriptions(user_id,ends_at);
CREATE TABLE portal_ledger(id TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES portal_users(id),kind TEXT NOT NULL,amount INTEGER NOT NULL,balance INTEGER NOT NULL,reference TEXT NOT NULL UNIQUE,note TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE portal_orders(id TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES portal_users(id),amount INTEGER NOT NULL CHECK(amount>0),channel TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'pending',merchant_id TEXT NOT NULL,config_id TEXT NOT NULL,trade_no TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,paid_at TEXT NOT NULL DEFAULT '');
CREATE UNIQUE INDEX portal_orders_trade ON portal_orders(config_id,trade_no) WHERE trade_no<>'';
CREATE TABLE portal_billing(id TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES portal_users(id),model TEXT NOT NULL,input_rate INTEGER NOT NULL,output_rate INTEGER NOT NULL,reserved INTEGER NOT NULL,subscription_id TEXT NOT NULL DEFAULT '',subscription_hold INTEGER NOT NULL DEFAULT 0,wallet_hold INTEGER NOT NULL DEFAULT 0,status TEXT NOT NULL DEFAULT 'reserved',charge INTEGER,input_tokens INTEGER,output_tokens INTEGER,note TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,finished_at TEXT NOT NULL DEFAULT '');
CREATE INDEX portal_billing_user ON portal_billing(user_id,created_at);
CREATE TABLE portal_access_keys(api_key_id TEXT PRIMARY KEY REFERENCES api_keys(id),user_id TEXT NOT NULL REFERENCES portal_users(id),created_at TEXT NOT NULL);
CREATE INDEX portal_access_keys_user ON portal_access_keys(user_id);
CREATE INDEX portal_orders_user_created ON portal_orders(user_id,created_at);
CREATE INDEX portal_ledger_user_created ON portal_ledger(user_id,created_at);
CREATE INDEX portal_billing_review ON portal_billing(status,created_at);
`,
	},
	{
		// 支付去重使用稳定的网关与商户范围，不随配置快照或密钥轮换改变。
		filename: "012_payment_trade_scope.sql",
		sql: `
ALTER TABLE portal_orders ADD COLUMN payment_scope TEXT NOT NULL DEFAULT '';
CREATE INDEX portal_orders_scoped_trade ON portal_orders(payment_scope,trade_no) WHERE trade_no<>'';
`,
	},
	{filename: "013_personal_edition.sql", sql: `
CREATE TEMP TABLE retired_personal_keys AS SELECT api_key_id FROM portal_user_keys UNION SELECT api_key_id FROM portal_access_keys;
DROP TABLE portal_access_keys;
DROP TABLE portal_user_keys;
DROP TABLE portal_billing;
DROP TABLE portal_orders;
DROP TABLE portal_ledger;
DROP TABLE portal_subscriptions;
DROP TABLE portal_plans;
DROP TABLE portal_wallets;
DROP TABLE model_prices;
DROP TABLE portal_daily_usage;
DROP TABLE portal_sessions;
DROP TABLE portal_users;
DELETE FROM api_keys WHERE id IN (SELECT api_key_id FROM retired_personal_keys);
DROP TABLE retired_personal_keys;
DELETE FROM app_secrets WHERE name LIKE 'commerce_payment_%' OR name LIKE '%tunnel%';
DELETE FROM settings WHERE key LIKE 'commerce_%' OR key LIKE 'payment_%' OR key LIKE 'portal_%' OR key LIKE '%tunnel%' OR key='network_profile_v1';
`},
}
