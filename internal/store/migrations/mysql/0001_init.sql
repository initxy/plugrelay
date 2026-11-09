-- MySQL 8.0.13+ / MariaDB 10.6+. Times are Unix milliseconds (0 = unset).
-- Money is in micro-units of the site currency.
--
-- Keys and indexed columns are VARCHAR (InnoDB indexes at most 3072 bytes,
-- 4 per utf8mb4 character), other text is TEXT or LONGTEXT, whose defaults
-- must be parenthesized expressions. utf8mb4_bin keeps comparisons
-- case-sensitive as in SQLite and PostgreSQL. JSON stays LONGTEXT: the
-- JSON type reformats, and raw usage must stay verbatim. "groups" and
-- "usage" are reserved words in MySQL.

-- ---------------------------------------------------------------- routing version

-- version is bumped by every write that changes what the relay routes on,
-- so nodes know when to reload their snapshot.
CREATE TABLE meta (
  k VARCHAR(64) NOT NULL PRIMARY KEY,
  v BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
INSERT INTO meta (k, v) VALUES ('version', 1);

-- ---------------------------------------------------------------- users and sign-in

-- price_multiplier scales the list price of a group's usage.
CREATE TABLE user_groups (
  id VARCHAR(128) NOT NULL PRIMARY KEY,
  name TEXT NOT NULL,
  settings LONGTEXT NOT NULL DEFAULT ('{}'),
  price_multiplier DOUBLE NOT NULL DEFAULT 1,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- email_unverified marks an address nobody vouched for (self-service
-- sign-up, import): SSO link_by_email never links to it. email_key stands
-- in for a partial unique index: NULL (no email) never collides.
CREATE TABLE users (
  id VARCHAR(128) NOT NULL PRIMARY KEY,
  name TEXT NOT NULL,
  email VARCHAR(255) NOT NULL DEFAULT '',
  email_unverified BOOLEAN NOT NULL DEFAULT FALSE,
  password_hash TEXT NOT NULL DEFAULT (''),
  role VARCHAR(32) NOT NULL,
  group_id VARCHAR(128) NOT NULL DEFAULT '',
  disabled BOOLEAN NOT NULL DEFAULT FALSE,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL,
  email_key VARCHAR(255) GENERATED ALWAYS AS (NULLIF(email, '')) STORED,
  UNIQUE INDEX users_email (email_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE admin_sessions (
  token_hash VARCHAR(128) NOT NULL PRIMARY KEY,
  user_id VARCHAR(128) NOT NULL,
  expires_at BIGINT NOT NULL,
  created_at BIGINT NOT NULL,
  INDEX admin_sessions_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- External sign-in identities (SSO) linked to users.
CREATE TABLE user_identities (
  id VARCHAR(128) NOT NULL PRIMARY KEY,
  provider VARCHAR(128) NOT NULL,
  subject VARCHAR(255) NOT NULL,
  user_id VARCHAR(128) NOT NULL,
  email VARCHAR(255) NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL,
  last_login_at BIGINT NOT NULL DEFAULT 0,
  UNIQUE INDEX user_identities_subject (provider, subject),
  INDEX user_identities_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- Second sign-in factors: TOTP (secret sealed with the master key),
-- one-time recovery codes (hashed) and passkeys (WebAuthn credentials).
-- A passkey credential id is base64url of up to 1023 bytes: ASCII keeps
-- its unique index within InnoDB's key size.
CREATE TABLE user_totp (
  user_id VARCHAR(128) NOT NULL PRIMARY KEY,
  secret TEXT NOT NULL,
  last_step BIGINT NOT NULL DEFAULT 0,
  created_at BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE user_recovery_codes (
  user_id VARCHAR(128) NOT NULL,
  code_hash VARCHAR(128) NOT NULL,
  created_at BIGINT NOT NULL,
  PRIMARY KEY (user_id, code_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE user_passkeys (
  id VARCHAR(128) NOT NULL PRIMARY KEY,
  user_id VARCHAR(128) NOT NULL,
  credential_id VARCHAR(1400) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  public_key LONGTEXT NOT NULL,
  alg BIGINT NOT NULL,
  sign_count BIGINT NOT NULL DEFAULT 0,
  name TEXT NOT NULL DEFAULT (''),
  transports TEXT NOT NULL DEFAULT (''),
  created_at BIGINT NOT NULL,
  last_used_at BIGINT NOT NULL DEFAULT 0,
  UNIQUE INDEX user_passkeys_credential (credential_id),
  INDEX user_passkeys_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- ---------------------------------------------------------------- API keys

-- Only the hash of a key is stored, and its last four characters to show.
CREATE TABLE api_keys (
  id VARCHAR(128) NOT NULL PRIMARY KEY,
  name TEXT NOT NULL,
  hash VARCHAR(128) NOT NULL,
  last4 VARCHAR(32) NOT NULL,
  user_id VARCHAR(128) NOT NULL DEFAULT '',
  group_id VARCHAR(128) NOT NULL DEFAULT '',
  settings LONGTEXT NOT NULL DEFAULT ('{}'),
  exceptions LONGTEXT NOT NULL DEFAULT ('[]'),
  disabled BOOLEAN NOT NULL DEFAULT FALSE,
  expires_at BIGINT NOT NULL DEFAULT 0,
  created_at BIGINT NOT NULL,
  last_used_at BIGINT NOT NULL DEFAULT 0,
  UNIQUE INDEX api_keys_hash (hash),
  INDEX api_keys_user (user_id),
  INDEX api_keys_group (group_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- ---------------------------------------------------------------- upstreams and routing

-- Upstream manifests (YAML) added by the operator or bundled.
CREATE TABLE manifests (
  id VARCHAR(191) NOT NULL PRIMARY KEY,
  yaml LONGTEXT NOT NULL,
  source VARCHAR(32) NOT NULL DEFAULT 'custom',
  updated_at BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE upstreams (
  id VARCHAR(128) NOT NULL PRIMARY KEY,
  provider VARCHAR(128) NOT NULL,
  manifest VARCHAR(191) NOT NULL DEFAULT '',
  endpoints LONGTEXT NOT NULL DEFAULT ('[]'),
  config LONGTEXT NOT NULL DEFAULT ('{}'),
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  source VARCHAR(32) NOT NULL DEFAULT 'custom',
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- secret is sealed with the master key. status is the credential's
-- rotation state (active, cooling, quarantined, ...).
CREATE TABLE credentials (
  id VARCHAR(128) NOT NULL PRIMARY KEY,
  upstream_id VARCHAR(128) NOT NULL,
  kind VARCHAR(64) NOT NULL,
  owner_id VARCHAR(128) NOT NULL DEFAULT '',
  secret LONGTEXT NOT NULL DEFAULT (''),
  proxy TEXT NOT NULL DEFAULT (''),
  max_concurrency BIGINT NOT NULL DEFAULT 0,
  status VARCHAR(32) NOT NULL DEFAULT 'active',
  status_reason TEXT NOT NULL DEFAULT (''),
  status_until BIGINT NOT NULL DEFAULT 0,
  meta LONGTEXT NOT NULL DEFAULT ('{}'),
  expires_at BIGINT NOT NULL DEFAULT 0,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL,
  INDEX credentials_upstream (upstream_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- The models clients ask for. manual_disabled is set when an operator
-- turned the model off, so model sync never turns it back on.
CREATE TABLE models (
  name VARCHAR(191) NOT NULL PRIMARY KEY,
  info LONGTEXT NOT NULL DEFAULT ('{}'),
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  manual_disabled BOOLEAN NOT NULL DEFAULT FALSE,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- A preset is a model name served by one of several member models.
CREATE TABLE presets (
  name VARCHAR(191) NOT NULL PRIMARY KEY,
  models LONGTEXT NOT NULL DEFAULT ('[]'),
  strategy VARCHAR(32) NOT NULL DEFAULT 'in_order',
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- Which upstream model serves a model. Higher priority is tried first.
-- weight splits new sessions within a priority. groups limits a route to
-- keys of those groups (empty: all).
CREATE TABLE routes (
  model VARCHAR(191) NOT NULL,
  upstream_id VARCHAR(128) NOT NULL,
  upstream_model VARCHAR(191) NOT NULL,
  priority BIGINT NOT NULL DEFAULT 0,
  weight BIGINT NOT NULL DEFAULT 1,
  `groups` LONGTEXT NOT NULL DEFAULT ('[]'),
  PRIMARY KEY (model, upstream_id, upstream_model),
  INDEX routes_upstream (upstream_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- ---------------------------------------------------------------- prices

-- A rule keyed by model is a list price, one keyed by upstream and
-- upstream model is the operator's cost. Token prices are micro-units of
-- currency per million tokens. units maps a media unit (image, audio
-- second, characters) to micro-units per unit. batch_discount is the
-- fraction taken off batch usage (0.5 = half price). source is manual or
-- sync.
CREATE TABLE price_rules (
  model VARCHAR(191) NOT NULL DEFAULT '',
  upstream_id VARCHAR(128) NOT NULL DEFAULT '',
  upstream_model VARCHAR(191) NOT NULL DEFAULT '',
  input BIGINT NOT NULL DEFAULT 0,
  cache_read BIGINT NOT NULL DEFAULT 0,
  cache_write_5m BIGINT NOT NULL DEFAULT 0,
  cache_write_1h BIGINT NOT NULL DEFAULT 0,
  output BIGINT NOT NULL DEFAULT 0,
  units LONGTEXT NOT NULL DEFAULT ('{}'),
  batch_discount DOUBLE NOT NULL DEFAULT 0,
  currency VARCHAR(16) NOT NULL DEFAULT 'USD',
  source VARCHAR(16) NOT NULL DEFAULT 'manual',
  updated_at BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (model, upstream_id, upstream_model)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- What the public model catalog (models.dev) says about one of our models,
-- and the operator's decisions about it: follow applies new catalog prices
-- unless a manual price exists, rejected holds the prices last rejected.
CREATE TABLE price_sync (
  model VARCHAR(191) NOT NULL PRIMARY KEY,
  follow BOOLEAN NOT NULL DEFAULT FALSE,
  ref VARCHAR(255) NOT NULL DEFAULT '',
  input BIGINT NOT NULL DEFAULT 0,
  cache_read BIGINT NOT NULL DEFAULT 0,
  cache_write_5m BIGINT NOT NULL DEFAULT 0,
  cache_write_1h BIGINT NOT NULL DEFAULT 0,
  output BIGINT NOT NULL DEFAULT 0,
  currency VARCHAR(16) NOT NULL DEFAULT 'USD',
  info LONGTEXT NOT NULL DEFAULT ('{}'),
  fetched_at BIGINT NOT NULL DEFAULT 0,
  rejected VARCHAR(255) NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- ---------------------------------------------------------------- features, plugins, settings

-- Features switched on or off by an operator. A feature without a row is
-- at its default (internal/features).
CREATE TABLE features (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  enabled BOOLEAN NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- The database part of a plugin's configuration (relay.yaml entries take
-- precedence). ord orders the plugins.
CREATE TABLE plugin_configs (
  plugin_id VARCHAR(128) NOT NULL PRIMARY KEY,
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  ord BIGINT NOT NULL DEFAULT 0,
  config LONGTEXT NOT NULL DEFAULT ('{}')
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- Setting values in layers: scope is global, group, user or key, and
-- scope_id is empty only for global.
CREATE TABLE settings (
  scope VARCHAR(32) NOT NULL,
  scope_id VARCHAR(128) NOT NULL DEFAULT '',
  plugin_id VARCHAR(128) NOT NULL,
  setting_key VARCHAR(191) NOT NULL,
  value LONGTEXT NOT NULL,
  updated_at BIGINT NOT NULL,
  PRIMARY KEY (scope, scope_id, plugin_id, setting_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- Plugin key-value store. num holds a value that is an integer, so
-- increments are a single upsert.
CREATE TABLE plugin_kv (
  plugin_id VARCHAR(128) NOT NULL,
  k VARCHAR(512) NOT NULL,
  value LONGBLOB,
  num BIGINT,
  expires_at BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (plugin_id, k)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- ---------------------------------------------------------------- request log and usage

-- One row per request. client is the client program (pkg/clientid).
-- cause is a failed request's final failure cause. attempts, usage and body
-- are JSON kept verbatim. client is VARCHAR(48) so the usage_hourly key
-- stays under InnoDB's 3072-byte limit.
CREATE TABLE request_log (
  id VARCHAR(191) NOT NULL PRIMARY KEY,
  time BIGINT NOT NULL,
  key_id VARCHAR(128) NOT NULL DEFAULT '',
  key_name TEXT NOT NULL DEFAULT (''),
  user_id VARCHAR(128) NOT NULL DEFAULT '',
  group_id VARCHAR(128) NOT NULL DEFAULT '',
  protocol VARCHAR(64) NOT NULL DEFAULT '',
  model VARCHAR(191) NOT NULL DEFAULT '',
  stream BOOLEAN NOT NULL DEFAULT FALSE,
  session_id TEXT NOT NULL DEFAULT (''),
  client_ua TEXT NOT NULL DEFAULT (''),
  client_ip TEXT NOT NULL DEFAULT (''),
  client VARCHAR(48) NOT NULL DEFAULT '',
  client_version VARCHAR(48) NOT NULL DEFAULT '',
  status BIGINT NOT NULL DEFAULT 0,
  error_kind TEXT NOT NULL DEFAULT (''),
  error LONGTEXT NOT NULL DEFAULT (''),
  cause VARCHAR(32) NOT NULL DEFAULT '',
  upstream_id VARCHAR(128) NOT NULL DEFAULT '',
  upstream_model TEXT NOT NULL DEFAULT (''),
  tier TEXT NOT NULL DEFAULT (''),
  attempt_count BIGINT NOT NULL DEFAULT 0,
  retried BOOLEAN NOT NULL DEFAULT FALSE,
  estimated BOOLEAN NOT NULL DEFAULT FALSE,
  input_tokens BIGINT NOT NULL DEFAULT 0,
  cache_read_tokens BIGINT NOT NULL DEFAULT 0,
  cache_write_5m_tokens BIGINT NOT NULL DEFAULT 0,
  cache_write_1h_tokens BIGINT NOT NULL DEFAULT 0,
  output_tokens BIGINT NOT NULL DEFAULT 0,
  reasoning_tokens BIGINT NOT NULL DEFAULT 0,
  cost_micro BIGINT NOT NULL DEFAULT 0,
  charge_micro BIGINT NOT NULL DEFAULT 0,
  ttfb_ms BIGINT NOT NULL DEFAULT 0,
  duration_ms BIGINT NOT NULL DEFAULT 0,
  attempts LONGTEXT NOT NULL DEFAULT ('[]'),
  `usage` LONGTEXT NOT NULL DEFAULT ('{}'),
  body LONGTEXT,
  INDEX request_log_time (time, id),
  INDEX request_log_key (key_id, time),
  INDEX request_log_user (user_id, time),
  INDEX request_log_group (group_id, time),
  INDEX request_log_model (model, time),
  INDEX request_log_client (client, time),
  INDEX request_log_session (session_id(191), time)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- Hourly roll-ups of request_log, kept when log rows are pruned. bucket is
-- the hour's start.
CREATE TABLE usage_hourly (
  bucket BIGINT NOT NULL,
  key_id VARCHAR(128) NOT NULL,
  user_id VARCHAR(128) NOT NULL,
  group_id VARCHAR(128) NOT NULL,
  model VARCHAR(191) NOT NULL,
  upstream_id VARCHAR(128) NOT NULL,
  client VARCHAR(48) NOT NULL DEFAULT '',
  requests BIGINT NOT NULL DEFAULT 0,
  errors BIGINT NOT NULL DEFAULT 0,
  input_tokens BIGINT NOT NULL DEFAULT 0,
  cache_read_tokens BIGINT NOT NULL DEFAULT 0,
  cache_write_5m_tokens BIGINT NOT NULL DEFAULT 0,
  cache_write_1h_tokens BIGINT NOT NULL DEFAULT 0,
  output_tokens BIGINT NOT NULL DEFAULT 0,
  reasoning_tokens BIGINT NOT NULL DEFAULT 0,
  cost_micro BIGINT NOT NULL DEFAULT 0,
  charge_micro BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (bucket, key_id, user_id, group_id, model, upstream_id, client),
  INDEX usage_hourly_key (key_id, bucket),
  INDEX usage_hourly_user (user_id, bucket),
  INDEX usage_hourly_group (group_id, bucket)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE audit_log (
  id VARCHAR(128) NOT NULL PRIMARY KEY,
  time BIGINT NOT NULL,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT (''),
  diff LONGTEXT NOT NULL DEFAULT ('null'),
  INDEX audit_log_time (time, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- ---------------------------------------------------------------- batches

-- Stateful upstream objects created through the relay (batch input and
-- output files, OpenAI batches, Anthropic message batches). Each lives on
-- one upstream credential and belongs to the user (or key) that made it.
-- served_model is the member of a requested preset (model) the object was
-- placed on, empty when a model was requested. group_id is the creator's
-- group (payer and price multiplier). hold_id is the id its money is held
-- and charged under. input_tokens and output_tokens estimate an input
-- file. checked_at is when the relay last asked the upstream about an
-- unbilled batch.
CREATE TABLE batch_objects (
  kind VARCHAR(32) NOT NULL,
  id VARCHAR(191) NOT NULL,
  upstream_id VARCHAR(128) NOT NULL,
  endpoint_id VARCHAR(191) NOT NULL,
  credential_id VARCHAR(128) NOT NULL,
  protocol VARCHAR(64) NOT NULL,
  model VARCHAR(191) NOT NULL DEFAULT '',
  served_model VARCHAR(191) NOT NULL DEFAULT '',
  upstream_model TEXT NOT NULL DEFAULT (''),
  user_id VARCHAR(128) NOT NULL DEFAULT '',
  key_id VARCHAR(128) NOT NULL DEFAULT '',
  group_id VARCHAR(128) NOT NULL DEFAULT '',
  role VARCHAR(32) NOT NULL DEFAULT '',
  batch_id VARCHAR(191) NOT NULL DEFAULT '',
  object LONGTEXT NOT NULL DEFAULT (''),
  hold_id VARCHAR(191) NOT NULL DEFAULT '',
  input_tokens BIGINT NOT NULL DEFAULT 0,
  output_tokens BIGINT NOT NULL DEFAULT 0,
  created_at BIGINT NOT NULL,
  checked_at BIGINT NOT NULL DEFAULT 0,
  billed_at BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (kind, id),
  INDEX batch_objects_owner (kind, user_id, key_id, created_at),
  INDEX batch_objects_unbilled (billed_at, checked_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- ---------------------------------------------------------------- balances (sell kit)

-- Prepaid balances. held_micro is the sum of open request holds
-- (available = balance - held).
CREATE TABLE balances (
  subject VARCHAR(191) NOT NULL PRIMARY KEY,
  balance_micro BIGINT NOT NULL DEFAULT 0,
  held_micro BIGINT NOT NULL DEFAULT 0,
  credit_limit_micro BIGINT NOT NULL DEFAULT 0,
  updated_at BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- One row per balance change, whose id is the idempotency key
-- (charge:<request>, topup:<order>, redeem:<code>:<subject>, ...).
CREATE TABLE balance_txns (
  id VARCHAR(255) NOT NULL PRIMARY KEY,
  subject VARCHAR(191) NOT NULL,
  kind VARCHAR(32) NOT NULL,
  amount_micro BIGINT NOT NULL,
  balance_after_micro BIGINT NOT NULL DEFAULT 0,
  request_id VARCHAR(191) NOT NULL DEFAULT '',
  ref TEXT NOT NULL DEFAULT (''),
  note TEXT NOT NULL DEFAULT (''),
  actor TEXT NOT NULL DEFAULT (''),
  created_at BIGINT NOT NULL,
  INDEX balance_txns_subject (subject, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- Open reservations, each row being the request's settle/refund marker.
CREATE TABLE balance_holds (
  request_id VARCHAR(191) NOT NULL PRIMARY KEY,
  subject VARCHAR(191) NOT NULL,
  amount_micro BIGINT NOT NULL,
  created_at BIGINT NOT NULL,
  INDEX balance_holds_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- Redeem codes: only the SHA-256 of a code is stored.
CREATE TABLE redeem_codes (
  id VARCHAR(128) NOT NULL PRIMARY KEY,
  code_hash VARCHAR(128) NOT NULL,
  last4 VARCHAR(32) NOT NULL DEFAULT '',
  batch VARCHAR(191) NOT NULL DEFAULT '',
  amount_micro BIGINT NOT NULL,
  max_uses BIGINT NOT NULL DEFAULT 1,
  uses BIGINT NOT NULL DEFAULT 0,
  expires_at BIGINT NOT NULL DEFAULT 0,
  disabled BOOLEAN NOT NULL DEFAULT FALSE,
  created_by TEXT NOT NULL DEFAULT (''),
  created_at BIGINT NOT NULL,
  UNIQUE INDEX redeem_codes_hash (code_hash),
  INDEX redeem_codes_batch (batch, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE redeem_uses (
  code_id VARCHAR(128) NOT NULL,
  subject VARCHAR(191) NOT NULL,
  created_at BIGINT NOT NULL,
  PRIMARY KEY (code_id, subject)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- ---------------------------------------------------------------- subscriptions (sell kit)

-- One row per subscription, the plan terms copied in when it is created.
-- allowance, used and held are in the plan unit: micro-units of the site
-- currency ("money") or tokens. A user has at most one live (pending or
-- active) subscription. refund_due marks a payment that arrived for a
-- subscription that could not start.
CREATE TABLE subscriptions (
  id VARCHAR(128) NOT NULL PRIMARY KEY,
  user_id VARCHAR(128) NOT NULL,
  payer VARCHAR(191) NOT NULL,
  plan_id VARCHAR(128) NOT NULL,
  plan_name TEXT NOT NULL DEFAULT (''),
  status VARCHAR(16) NOT NULL,
  period VARCHAR(16) NOT NULL,
  price_micro BIGINT NOT NULL DEFAULT 0,
  unit VARCHAR(16) NOT NULL,
  allowance BIGINT NOT NULL DEFAULT 0,
  used BIGINT NOT NULL DEFAULT 0,
  held BIGINT NOT NULL DEFAULT 0,
  overage VARCHAR(16) NOT NULL,
  group_id VARCHAR(128) NOT NULL DEFAULT '',
  prev_group_id VARCHAR(128) NOT NULL DEFAULT '',
  period_start BIGINT NOT NULL DEFAULT 0,
  period_end BIGINT NOT NULL DEFAULT 0,
  cancel_at_period_end BOOLEAN NOT NULL DEFAULT FALSE,
  provider VARCHAR(32) NOT NULL DEFAULT '',
  renewal VARCHAR(16) NOT NULL DEFAULT '',
  provider_ref VARCHAR(191) NOT NULL DEFAULT '',
  refund_due BOOLEAN NOT NULL DEFAULT FALSE,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL,
  live_user VARCHAR(128) AS (CASE WHEN status IN ('pending', 'active') THEN user_id END) STORED,
  UNIQUE INDEX subscriptions_live (live_user),
  INDEX subscriptions_user (user_id, status, period_end),
  INDEX subscriptions_due (status, period_end),
  INDEX subscriptions_ref (provider, provider_ref)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- A draw of a request on a subscription allowance, the settle/release
-- marker (like balance_holds).
CREATE TABLE subscription_holds (
  request_id VARCHAR(191) NOT NULL PRIMARY KEY,
  subscription_id VARCHAR(128) NOT NULL,
  payer VARCHAR(191) NOT NULL,
  period_start BIGINT NOT NULL,
  amount BIGINT NOT NULL,
  created_at BIGINT NOT NULL,
  INDEX subscription_holds_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- ---------------------------------------------------------------- payments, coupons, referrals (sell kit)

-- Top-ups and plan periods bought at a payment provider. amount_micro is
-- what the order is worth (credited for a top-up, the plan's price),
-- pay_micro what the provider charges, less a coupon's discount_micro.
CREATE TABLE payment_orders (
  id VARCHAR(128) NOT NULL PRIMARY KEY,
  kind VARCHAR(16) NOT NULL,
  user_id VARCHAR(128) NOT NULL,
  subject VARCHAR(191) NOT NULL,
  amount_micro BIGINT NOT NULL,
  pay_micro BIGINT NOT NULL,
  discount_micro BIGINT NOT NULL DEFAULT 0,
  currency VARCHAR(8) NOT NULL,
  provider VARCHAR(32) NOT NULL,
  method VARCHAR(32) NOT NULL DEFAULT '',
  session_id VARCHAR(191) NOT NULL DEFAULT '',
  coupon_id VARCHAR(128) NOT NULL DEFAULT '',
  coupon_code VARCHAR(64) NOT NULL DEFAULT '',
  ref VARCHAR(191) NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT (''),
  status VARCHAR(16) NOT NULL,
  created_at BIGINT NOT NULL,
  paid_at BIGINT NOT NULL DEFAULT 0,
  INDEX payment_orders_created (created_at, id),
  INDEX payment_orders_user (user_id, created_at),
  INDEX payment_orders_status (status, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- A percent or fixed discount on top-ups. A use is taken at checkout
-- (pending) and kept once its order is paid. A pending use stops counting
-- after an hour and is deleted when its order is canceled.
CREATE TABLE coupons (
  id VARCHAR(128) NOT NULL PRIMARY KEY,
  code VARCHAR(64) NOT NULL,
  percent INTEGER NOT NULL DEFAULT 0,
  amount_micro BIGINT NOT NULL DEFAULT 0,
  max_discount_micro BIGINT NOT NULL DEFAULT 0,
  min_topup_micro BIGINT NOT NULL DEFAULT 0,
  max_uses INTEGER NOT NULL DEFAULT 0,
  per_user INTEGER NOT NULL DEFAULT 1,
  starts_at BIGINT NOT NULL DEFAULT 0,
  ends_at BIGINT NOT NULL DEFAULT 0,
  disabled BOOLEAN NOT NULL DEFAULT FALSE,
  note TEXT NOT NULL DEFAULT (''),
  created_by VARCHAR(128) NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL,
  UNIQUE INDEX coupons_code (code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE coupon_uses (
  order_id VARCHAR(128) NOT NULL PRIMARY KEY,
  coupon_id VARCHAR(128) NOT NULL,
  user_id VARCHAR(128) NOT NULL,
  discount_micro BIGINT NOT NULL,
  status VARCHAR(16) NOT NULL,
  created_at BIGINT NOT NULL,
  INDEX coupon_uses_coupon (coupon_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- Each user's invite code, and who invited whom. A referral is rewarded
-- once, on the invitee's first paid top-up (reward_micro may be 0 when the
-- inviter's cap was reached).
CREATE TABLE referral_codes (
  user_id VARCHAR(128) NOT NULL PRIMARY KEY,
  code VARCHAR(64) NOT NULL,
  created_at BIGINT NOT NULL,
  UNIQUE INDEX referral_codes_code (code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE referrals (
  invitee_id VARCHAR(128) NOT NULL PRIMARY KEY,
  inviter_id VARCHAR(128) NOT NULL,
  created_at BIGINT NOT NULL,
  reward_micro BIGINT NOT NULL DEFAULT 0,
  rewarded_at BIGINT NOT NULL DEFAULT 0,
  order_id VARCHAR(128) NOT NULL DEFAULT '',
  INDEX referrals_inviter (inviter_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- ---------------------------------------------------------------- notifications

-- Where notifications go (mailboxes, chat bots, webhooks). secrets is JSON
-- sealed under the master key.
CREATE TABLE notify_channels (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  name VARCHAR(191) NOT NULL,
  type VARCHAR(32) NOT NULL,
  enabled BOOLEAN NOT NULL,
  events TEXT NOT NULL DEFAULT (''),
  language VARCHAR(16) NOT NULL DEFAULT '',
  config TEXT NOT NULL DEFAULT (''),
  secrets TEXT NOT NULL DEFAULT (''),
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- The log of notifications sent and their deliveries per channel.
CREATE TABLE notifications (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  created_at BIGINT NOT NULL,
  event VARCHAR(64) NOT NULL,
  status VARCHAR(16) NOT NULL,
  objects LONGTEXT NOT NULL DEFAULT (''),
  deliveries TEXT NOT NULL DEFAULT (''),
  INDEX notifications_created (created_at),
  INDEX notifications_status (status, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
