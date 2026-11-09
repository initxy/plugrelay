-- A new-api database as gorm creates it on SQLite (model/channel.go,
-- model/token.go, model/user.go, model/option.go at 2de51a19), reduced to
-- the columns plugrelay reads plus a few it ignores.
CREATE TABLE channels (id integer PRIMARY KEY AUTOINCREMENT, type integer DEFAULT 0, key text NOT NULL, openai_organization text,
  status integer DEFAULT 1, name text, weight integer DEFAULT 0, created_time integer, base_url text DEFAULT '', other text,
  balance real, models text, "group" varchar(64) DEFAULT 'default', used_quota integer DEFAULT 0, model_mapping text,
  priority integer DEFAULT 0, auto_ban integer DEFAULT 1, tag text, setting text, settings text);
CREATE TABLE tokens (id integer PRIMARY KEY AUTOINCREMENT, user_id integer, key varchar(128), status integer DEFAULT 1, name text,
  created_time integer, accessed_time integer, expired_time integer DEFAULT -1, remain_quota integer DEFAULT 0,
  unlimited_quota numeric, model_limits_enabled numeric, model_limits text, allow_ips text DEFAULT '', used_quota integer DEFAULT 0,
  "group" text DEFAULT '', cross_group_retry numeric, auto_groups text, deleted_at datetime, allow_user_quota_query numeric);
CREATE TABLE users (id integer PRIMARY KEY AUTOINCREMENT, username text UNIQUE, password text NOT NULL, display_name text,
  role integer DEFAULT 1, status integer DEFAULT 1, email text, github_id text, quota integer DEFAULT 0, used_quota integer DEFAULT 0,
  request_count integer DEFAULT 0, "group" varchar(64) DEFAULT 'default', aff_code varchar(32), inviter_id integer,
  deleted_at datetime, setting text, remark varchar(255), created_at integer, last_login_at integer DEFAULT 0);
CREATE TABLE options (key text PRIMARY KEY, value text);

INSERT INTO options VALUES ('QuotaPerUnit', '500000'), ('GroupRatio', '{"default":1,"vip":0.8}');

INSERT INTO users (id, username, password, display_name, role, status, email, quota, "group") VALUES
  (1, 'root', '$2a$10$GhvpdkbCvM9XhJt6FfDRwOKusIwaOTsc//PsWlqUZwutlZfCTpoNW', 'Root', 100, 1, '', 0, 'default'),
  (2, 'alice', '$2a$10$GhvpdkbCvM9XhJt6FfDRwOKusIwaOTsc//PsWlqUZwutlZfCTpoNW', 'Alice', 1, 1, 'alice@example.com', 5000000, 'vip'),
  (3, 'bob', '$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA', '', 1, 2, 'bob@example.com', 250000, 'default');
INSERT INTO users (id, username, password, email, deleted_at) VALUES (4, 'gone', 'x', 'gone@example.com', '2025-01-01 00:00:00');

INSERT INTO channels (id, type, key, status, name, weight, base_url, models, "group", model_mapping, priority) VALUES
  (1, 1, 'sk-openai-1
sk-openai-2', 1, 'OpenAI', 10, '', 'gpt-4o,gpt-4o-mini', 'default,vip', '{"gpt-4o-mini":"gpt-4o-mini-2024-07-18"}', 5),
  (2, 1, 'sk-relay', 1, 'Another relay', 0, 'https://relay.example.net', 'gpt-4o', 'vip', '', 0),
  (3, 14, 'sk-ant-1', 2, 'Claude', 1, '', 'claude-sonnet-4-5', 'default', NULL, 0),
  (4, 2, 'mj-key', 1, 'Midjourney', 0, '', 'mj', 'default', NULL, 0);

INSERT INTO tokens (id, user_id, key, status, name, expired_time, remain_quota, unlimited_quota, model_limits_enabled, model_limits, allow_ips, "group") VALUES
  (1, 2, 'AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfGhIjKl', 1, 'alice laptop', -1, 0, 1, 1, 'gpt-4o', '10.0.0.0/8', ''),
  (2, 3, 'ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210ZyXwVuTsRqPo', 1, 'bob ci', 1893456000, 1000, 0, 0, '', '', 'vip'),
  (3, 4, 'GoneGoneGoneGoneGoneGoneGoneGoneGoneGoneGoneGone', 1, 'deleted user', -1, 0, 1, 0, '', '', ''),
  (4, 2, 'short', 1, 'malformed', -1, 0, 1, 0, '', '', '');
