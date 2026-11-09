-- Times are Unix milliseconds (0 = unset). Money is in micro-units of the
-- site currency. JSON columns hold text, kept as written.

-- ---------------------------------------------------------------- routing version

-- version is bumped by every write that changes what the relay routes on,
-- so nodes know when to reload their snapshot.
CREATE TABLE meta (
  k TEXT PRIMARY KEY,
  v INTEGER NOT NULL
);
INSERT INTO meta (k, v) VALUES ('version', 1);

-- ---------------------------------------------------------------- users and sign-in

-- price_multiplier scales the list price of a group's usage.
CREATE TABLE user_groups (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  settings TEXT NOT NULL DEFAULT '{}',
  price_multiplier REAL NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- email_unverified marks an address nobody vouched for (self-service
-- sign-up, import): SSO link_by_email never links to it.
CREATE TABLE users (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  email TEXT NOT NULL DEFAULT '',
  email_unverified BOOLEAN NOT NULL DEFAULT 0,
  password_hash TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL,
  group_id TEXT NOT NULL DEFAULT '',
  disabled BOOLEAN NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX users_email ON users (email) WHERE email <> '';

CREATE TABLE admin_sessions (
  token_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX admin_sessions_user ON admin_sessions (user_id);

-- External sign-in identities (SSO) linked to users.
CREATE TABLE user_identities (
  id TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  subject TEXT NOT NULL,
  user_id TEXT NOT NULL,
  email TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  last_login_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX user_identities_subject ON user_identities (provider, subject);
CREATE INDEX user_identities_user ON user_identities (user_id);

-- Second sign-in factors: TOTP (secret sealed with the master key),
-- one-time recovery codes (hashed) and passkeys (WebAuthn credentials).
CREATE TABLE user_totp (
  user_id TEXT PRIMARY KEY,
  secret TEXT NOT NULL,
  last_step INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE user_recovery_codes (
  user_id TEXT NOT NULL,
  code_hash TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, code_hash)
);

CREATE TABLE user_passkeys (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  credential_id TEXT NOT NULL,
  public_key TEXT NOT NULL,
  alg INTEGER NOT NULL,
  sign_count INTEGER NOT NULL DEFAULT 0,
  name TEXT NOT NULL DEFAULT '',
  transports TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX user_passkeys_credential ON user_passkeys (credential_id);
CREATE INDEX user_passkeys_user ON user_passkeys (user_id);

-- ---------------------------------------------------------------- API keys

-- Only the hash of a key is stored, and its last four characters to show.
CREATE TABLE api_keys (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  hash TEXT NOT NULL UNIQUE,
  last4 TEXT NOT NULL,
  user_id TEXT NOT NULL DEFAULT '',
  group_id TEXT NOT NULL DEFAULT '',
  settings TEXT NOT NULL DEFAULT '{}',
  exceptions TEXT NOT NULL DEFAULT '[]',
  disabled BOOLEAN NOT NULL DEFAULT 0,
  expires_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX api_keys_user ON api_keys (user_id);
CREATE INDEX api_keys_group ON api_keys (group_id);

-- ---------------------------------------------------------------- upstreams and routing

-- Upstream manifests (YAML) added by the operator or bundled.
CREATE TABLE manifests (
  id TEXT PRIMARY KEY,
  yaml TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT 'custom',
  updated_at INTEGER NOT NULL
);

CREATE TABLE upstreams (
  id TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  manifest TEXT NOT NULL DEFAULT '',
  endpoints TEXT NOT NULL DEFAULT '[]',
  config TEXT NOT NULL DEFAULT '{}',
  enabled BOOLEAN NOT NULL DEFAULT 1,
  source TEXT NOT NULL DEFAULT 'custom',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- secret is sealed with the master key. status is the credential's
-- rotation state (active, cooling, quarantined, ...).
CREATE TABLE credentials (
  id TEXT PRIMARY KEY,
  upstream_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  owner_id TEXT NOT NULL DEFAULT '',
  secret TEXT NOT NULL DEFAULT '',
  proxy TEXT NOT NULL DEFAULT '',
  max_concurrency INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'active',
  status_reason TEXT NOT NULL DEFAULT '',
  status_until INTEGER NOT NULL DEFAULT 0,
  meta TEXT NOT NULL DEFAULT '{}',
  expires_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX credentials_upstream ON credentials (upstream_id);

-- The models clients ask for. manual_disabled is set when an operator
-- turned the model off, so model sync never turns it back on.
CREATE TABLE models (
  name TEXT PRIMARY KEY,
  info TEXT NOT NULL DEFAULT '{}',
  enabled BOOLEAN NOT NULL DEFAULT 1,
  manual_disabled BOOLEAN NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- A preset is a model name served by one of several member models.
CREATE TABLE presets (
  name TEXT PRIMARY KEY,
  models TEXT NOT NULL DEFAULT '[]',
  strategy TEXT NOT NULL DEFAULT 'in_order',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- Which upstream model serves a model. Higher priority is tried first.
-- weight splits new sessions within a priority. groups limits a route to
-- keys of those groups (empty: all).
CREATE TABLE routes (
  model TEXT NOT NULL,
  upstream_id TEXT NOT NULL,
  upstream_model TEXT NOT NULL,
  priority INTEGER NOT NULL DEFAULT 0,
  weight INTEGER NOT NULL DEFAULT 1,
  groups TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY (model, upstream_id, upstream_model)
);
CREATE INDEX routes_upstream ON routes (upstream_id);

-- ---------------------------------------------------------------- prices

-- A rule keyed by model is a list price, one keyed by upstream and
-- upstream model is the operator's cost. Token prices are micro-units of
-- currency per million tokens. units maps a media unit (image, audio
-- second, characters) to micro-units per unit. batch_discount is the
-- fraction taken off batch usage (0.5 = half price). source is manual or
-- sync.
CREATE TABLE price_rules (
  model TEXT NOT NULL DEFAULT '',
  upstream_id TEXT NOT NULL DEFAULT '',
  upstream_model TEXT NOT NULL DEFAULT '',
  input INTEGER NOT NULL DEFAULT 0,
  cache_read INTEGER NOT NULL DEFAULT 0,
  cache_write_5m INTEGER NOT NULL DEFAULT 0,
  cache_write_1h INTEGER NOT NULL DEFAULT 0,
  output INTEGER NOT NULL DEFAULT 0,
  units TEXT NOT NULL DEFAULT '{}',
  batch_discount REAL NOT NULL DEFAULT 0,
  currency TEXT NOT NULL DEFAULT 'USD',
  source TEXT NOT NULL DEFAULT 'manual',
  updated_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (model, upstream_id, upstream_model)
);

-- What the public model catalog (models.dev) says about one of our models,
-- and the operator's decisions about it: follow applies new catalog prices
-- unless a manual price exists, rejected holds the prices last rejected.
CREATE TABLE price_sync (
  model TEXT PRIMARY KEY,
  follow BOOLEAN NOT NULL DEFAULT 0,
  ref TEXT NOT NULL DEFAULT '',
  input INTEGER NOT NULL DEFAULT 0,
  cache_read INTEGER NOT NULL DEFAULT 0,
  cache_write_5m INTEGER NOT NULL DEFAULT 0,
  cache_write_1h INTEGER NOT NULL DEFAULT 0,
  output INTEGER NOT NULL DEFAULT 0,
  currency TEXT NOT NULL DEFAULT 'USD',
  info TEXT NOT NULL DEFAULT '{}',
  fetched_at INTEGER NOT NULL DEFAULT 0,
  rejected TEXT NOT NULL DEFAULT ''
);

-- ---------------------------------------------------------------- features, plugins, settings

-- Features switched on or off by an operator. A feature without a row is
-- at its default (internal/features).
CREATE TABLE features (
  id TEXT PRIMARY KEY,
  enabled BOOLEAN NOT NULL
);

-- The database part of a plugin's configuration (relay.yaml entries take
-- precedence). ord orders the plugins.
CREATE TABLE plugin_configs (
  plugin_id TEXT PRIMARY KEY,
  enabled BOOLEAN NOT NULL DEFAULT 1,
  ord INTEGER NOT NULL DEFAULT 0,
  config TEXT NOT NULL DEFAULT '{}'
);

-- Setting values in layers: scope is global, group, user or key, and
-- scope_id is empty only for global.
CREATE TABLE settings (
  scope TEXT NOT NULL,
  scope_id TEXT NOT NULL DEFAULT '',
  plugin_id TEXT NOT NULL,
  setting_key TEXT NOT NULL,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (scope, scope_id, plugin_id, setting_key)
);

-- Plugin key-value store. num holds a value that is an integer, so
-- increments are a single upsert.
CREATE TABLE plugin_kv (
  plugin_id TEXT NOT NULL,
  k TEXT NOT NULL,
  value BLOB,
  num INTEGER,
  expires_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (plugin_id, k)
);

-- ---------------------------------------------------------------- request log and usage

-- One row per request. client is the client program (pkg/clientid).
-- cause is a failed request's final failure cause. attempts, usage and body
-- are JSON kept verbatim.
CREATE TABLE request_log (
  id TEXT PRIMARY KEY,
  time INTEGER NOT NULL,
  key_id TEXT NOT NULL DEFAULT '',
  key_name TEXT NOT NULL DEFAULT '',
  user_id TEXT NOT NULL DEFAULT '',
  group_id TEXT NOT NULL DEFAULT '',
  protocol TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  stream BOOLEAN NOT NULL DEFAULT 0,
  session_id TEXT NOT NULL DEFAULT '',
  client_ua TEXT NOT NULL DEFAULT '',
  client_ip TEXT NOT NULL DEFAULT '',
  client TEXT NOT NULL DEFAULT '',
  client_version TEXT NOT NULL DEFAULT '',
  status INTEGER NOT NULL DEFAULT 0,
  error_kind TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  cause TEXT NOT NULL DEFAULT '',
  upstream_id TEXT NOT NULL DEFAULT '',
  upstream_model TEXT NOT NULL DEFAULT '',
  tier TEXT NOT NULL DEFAULT '',
  attempt_count INTEGER NOT NULL DEFAULT 0,
  retried BOOLEAN NOT NULL DEFAULT 0,
  estimated BOOLEAN NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_5m_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_1h_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens INTEGER NOT NULL DEFAULT 0,
  cost_micro INTEGER NOT NULL DEFAULT 0,
  charge_micro INTEGER NOT NULL DEFAULT 0,
  ttfb_ms INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  attempts TEXT NOT NULL DEFAULT '[]',
  usage TEXT NOT NULL DEFAULT '{}',
  body TEXT
);
CREATE INDEX request_log_time ON request_log (time, id);
CREATE INDEX request_log_key ON request_log (key_id, time);
CREATE INDEX request_log_user ON request_log (user_id, time);
CREATE INDEX request_log_group ON request_log (group_id, time);
CREATE INDEX request_log_model ON request_log (model, time);
CREATE INDEX request_log_client ON request_log (client, time);
CREATE INDEX request_log_session ON request_log (session_id, time);

-- Hourly roll-ups of request_log, kept when log rows are pruned. bucket is
-- the hour's start.
CREATE TABLE usage_hourly (
  bucket INTEGER NOT NULL,
  key_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  group_id TEXT NOT NULL,
  model TEXT NOT NULL,
  upstream_id TEXT NOT NULL,
  client TEXT NOT NULL DEFAULT '',
  requests INTEGER NOT NULL DEFAULT 0,
  errors INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_5m_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_1h_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens INTEGER NOT NULL DEFAULT 0,
  cost_micro INTEGER NOT NULL DEFAULT 0,
  charge_micro INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (bucket, key_id, user_id, group_id, model, upstream_id, client)
);
CREATE INDEX usage_hourly_key ON usage_hourly (key_id, bucket);
CREATE INDEX usage_hourly_user ON usage_hourly (user_id, bucket);
CREATE INDEX usage_hourly_group ON usage_hourly (group_id, bucket);

CREATE TABLE audit_log (
  id TEXT PRIMARY KEY,
  time INTEGER NOT NULL,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  diff TEXT NOT NULL DEFAULT 'null'
);
CREATE INDEX audit_log_time ON audit_log (time, id);

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
  kind TEXT NOT NULL,
  id TEXT NOT NULL,
  upstream_id TEXT NOT NULL,
  endpoint_id TEXT NOT NULL,
  credential_id TEXT NOT NULL,
  protocol TEXT NOT NULL,
  model TEXT NOT NULL DEFAULT '',
  served_model TEXT NOT NULL DEFAULT '',
  upstream_model TEXT NOT NULL DEFAULT '',
  user_id TEXT NOT NULL DEFAULT '',
  key_id TEXT NOT NULL DEFAULT '',
  group_id TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT '',
  batch_id TEXT NOT NULL DEFAULT '',
  object TEXT NOT NULL DEFAULT '',
  hold_id TEXT NOT NULL DEFAULT '',
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  checked_at INTEGER NOT NULL DEFAULT 0,
  billed_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (kind, id)
);
CREATE INDEX batch_objects_owner ON batch_objects (kind, user_id, key_id, created_at);
CREATE INDEX batch_objects_unbilled ON batch_objects (billed_at, checked_at);

-- ---------------------------------------------------------------- balances (sell kit)

-- Prepaid balances. held_micro is the sum of open request holds
-- (available = balance - held).
CREATE TABLE balances (
  subject TEXT PRIMARY KEY,
  balance_micro INTEGER NOT NULL DEFAULT 0,
  held_micro INTEGER NOT NULL DEFAULT 0,
  credit_limit_micro INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);

-- One row per balance change, whose id is the idempotency key
-- (charge:<request>, topup:<order>, redeem:<code>:<subject>, ...).
CREATE TABLE balance_txns (
  id TEXT PRIMARY KEY,
  subject TEXT NOT NULL,
  kind TEXT NOT NULL,
  amount_micro INTEGER NOT NULL,
  balance_after_micro INTEGER NOT NULL DEFAULT 0,
  request_id TEXT NOT NULL DEFAULT '',
  ref TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  actor TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX balance_txns_subject ON balance_txns (subject, created_at, id);

-- Open reservations, each row being the request's settle/refund marker.
CREATE TABLE balance_holds (
  request_id TEXT PRIMARY KEY,
  subject TEXT NOT NULL,
  amount_micro INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX balance_holds_created ON balance_holds (created_at);

-- Redeem codes: only the SHA-256 of a code is stored.
CREATE TABLE redeem_codes (
  id TEXT PRIMARY KEY,
  code_hash TEXT NOT NULL UNIQUE,
  last4 TEXT NOT NULL DEFAULT '',
  batch TEXT NOT NULL DEFAULT '',
  amount_micro INTEGER NOT NULL,
  max_uses INTEGER NOT NULL DEFAULT 1,
  uses INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER NOT NULL DEFAULT 0,
  disabled BOOLEAN NOT NULL DEFAULT 0,
  created_by TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX redeem_codes_batch ON redeem_codes (batch, created_at);

CREATE TABLE redeem_uses (
  code_id TEXT NOT NULL,
  subject TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (code_id, subject)
);

-- ---------------------------------------------------------------- subscriptions (sell kit)

-- One row per subscription, the plan terms copied in when it is created.
-- allowance, used and held are in the plan unit: micro-units of the site
-- currency ("money") or tokens. A user has at most one live (pending or
-- active) subscription. refund_due marks a payment that arrived for a
-- subscription that could not start.
CREATE TABLE subscriptions (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  payer TEXT NOT NULL,
  plan_id TEXT NOT NULL,
  plan_name TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  period TEXT NOT NULL,
  price_micro INTEGER NOT NULL DEFAULT 0,
  unit TEXT NOT NULL,
  allowance INTEGER NOT NULL DEFAULT 0,
  used INTEGER NOT NULL DEFAULT 0,
  held INTEGER NOT NULL DEFAULT 0,
  overage TEXT NOT NULL,
  group_id TEXT NOT NULL DEFAULT '',
  prev_group_id TEXT NOT NULL DEFAULT '',
  period_start INTEGER NOT NULL DEFAULT 0,
  period_end INTEGER NOT NULL DEFAULT 0,
  cancel_at_period_end BOOLEAN NOT NULL DEFAULT 0,
  provider TEXT NOT NULL DEFAULT '',
  renewal TEXT NOT NULL DEFAULT '',
  provider_ref TEXT NOT NULL DEFAULT '',
  refund_due BOOLEAN NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX subscriptions_live ON subscriptions (user_id) WHERE status IN ('pending', 'active');
CREATE INDEX subscriptions_user ON subscriptions (user_id, status, period_end);
CREATE INDEX subscriptions_due ON subscriptions (status, period_end);
CREATE INDEX subscriptions_ref ON subscriptions (provider, provider_ref);

-- A draw of a request on a subscription allowance, the settle/release
-- marker (like balance_holds).
CREATE TABLE subscription_holds (
  request_id TEXT PRIMARY KEY,
  subscription_id TEXT NOT NULL,
  payer TEXT NOT NULL,
  period_start INTEGER NOT NULL,
  amount INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX subscription_holds_created ON subscription_holds (created_at);

-- ---------------------------------------------------------------- payments, coupons, referrals (sell kit)

-- Top-ups and plan periods bought at a payment provider. amount_micro is
-- what the order is worth (credited for a top-up, the plan's price),
-- pay_micro what the provider charges, less a coupon's discount_micro.
CREATE TABLE payment_orders (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  user_id TEXT NOT NULL,
  subject TEXT NOT NULL,
  amount_micro INTEGER NOT NULL,
  pay_micro INTEGER NOT NULL,
  discount_micro INTEGER NOT NULL DEFAULT 0,
  currency TEXT NOT NULL,
  provider TEXT NOT NULL,
  method TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL DEFAULT '',
  coupon_id TEXT NOT NULL DEFAULT '',
  coupon_code TEXT NOT NULL DEFAULT '',
  ref TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  paid_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX payment_orders_created ON payment_orders (created_at, id);
CREATE INDEX payment_orders_user ON payment_orders (user_id, created_at);
CREATE INDEX payment_orders_status ON payment_orders (status, created_at);

-- A percent or fixed discount on top-ups. A use is taken at checkout
-- (pending) and kept once its order is paid. A pending use stops counting
-- after an hour and is deleted when its order is canceled.
CREATE TABLE coupons (
  id TEXT PRIMARY KEY,
  code TEXT NOT NULL UNIQUE,
  percent INTEGER NOT NULL DEFAULT 0,
  amount_micro INTEGER NOT NULL DEFAULT 0,
  max_discount_micro INTEGER NOT NULL DEFAULT 0,
  min_topup_micro INTEGER NOT NULL DEFAULT 0,
  max_uses INTEGER NOT NULL DEFAULT 0,
  per_user INTEGER NOT NULL DEFAULT 1,
  starts_at INTEGER NOT NULL DEFAULT 0,
  ends_at INTEGER NOT NULL DEFAULT 0,
  disabled BOOLEAN NOT NULL DEFAULT 0,
  note TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE coupon_uses (
  order_id TEXT PRIMARY KEY,
  coupon_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  discount_micro INTEGER NOT NULL,
  status TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX coupon_uses_coupon ON coupon_uses (coupon_id, created_at);

-- Each user's invite code, and who invited whom. A referral is rewarded
-- once, on the invitee's first paid top-up (reward_micro may be 0 when the
-- inviter's cap was reached).
CREATE TABLE referral_codes (
  user_id TEXT PRIMARY KEY,
  code TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL
);

CREATE TABLE referrals (
  invitee_id TEXT PRIMARY KEY,
  inviter_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  reward_micro INTEGER NOT NULL DEFAULT 0,
  rewarded_at INTEGER NOT NULL DEFAULT 0,
  order_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX referrals_inviter ON referrals (inviter_id, created_at);

-- ---------------------------------------------------------------- notifications

-- Where notifications go (mailboxes, chat bots, webhooks). secrets is JSON
-- sealed under the master key.
CREATE TABLE notify_channels (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  type TEXT NOT NULL,
  enabled BOOLEAN NOT NULL,
  events TEXT NOT NULL DEFAULT '',
  language TEXT NOT NULL DEFAULT '',
  config TEXT NOT NULL DEFAULT '',
  secrets TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- The log of notifications sent and their deliveries per channel.
CREATE TABLE notifications (
  id TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  event TEXT NOT NULL,
  status TEXT NOT NULL,
  objects TEXT NOT NULL DEFAULT '',
  deliveries TEXT NOT NULL DEFAULT ''
);
CREATE INDEX notifications_created ON notifications (created_at);
CREATE INDEX notifications_status ON notifications (status, created_at);
