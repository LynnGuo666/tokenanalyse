PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS devices (
    device_id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    platform TEXT NOT NULL,
    arch TEXT NOT NULL,
    agent_version TEXT NOT NULL,
    first_seen_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    metadata TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS pricing_versions (
    version_id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    created_at TEXT NOT NULL,
    model_count INTEGER NOT NULL DEFAULT 0,
    content_hash TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS model_pricing (
    version_id TEXT NOT NULL,
    model_id TEXT NOT NULL,
    display_name TEXT NOT NULL,
    input_cost_per_million TEXT NOT NULL,
    output_cost_per_million TEXT NOT NULL,
    cache_read_cost_per_million TEXT NOT NULL,
    cache_creation_cost_per_million TEXT NOT NULL,
    currency TEXT NOT NULL DEFAULT 'USD',
    source TEXT NOT NULL DEFAULT 'custom',
    PRIMARY KEY (version_id, model_id),
    FOREIGN KEY (version_id) REFERENCES pricing_versions(version_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS model_aliases (
    alias_id TEXT PRIMARY KEY,
    alias_pattern TEXT NOT NULL,
    canonical_model_id TEXT NOT NULL,
    match_type TEXT NOT NULL DEFAULT 'exact',
    scope_app TEXT NOT NULL DEFAULT '',
    priority INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    UNIQUE(alias_pattern, match_type, scope_app)
);

CREATE INDEX IF NOT EXISTS idx_model_aliases_lookup ON model_aliases(scope_app, match_type, priority DESC);

CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

INSERT OR IGNORE INTO settings(key, value) VALUES ('cny_exchange_rate', '7.2');
INSERT OR IGNORE INTO settings(key, value) VALUES ('active_pricing_version', '');

CREATE TABLE IF NOT EXISTS usage_events (
    event_id TEXT PRIMARY KEY,
    device_id TEXT NOT NULL,
    source TEXT NOT NULL,
    source_app TEXT NOT NULL,
    source_record_id TEXT NOT NULL,
    request_id TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    provider_id TEXT NOT NULL DEFAULT '',
    provider_name TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL,
    model_raw TEXT NOT NULL DEFAULT '',
    canonical_model TEXT NOT NULL DEFAULT '',
    request_model TEXT NOT NULL DEFAULT '',
    pricing_model TEXT NOT NULL DEFAULT '',
    occurred_at TEXT NOT NULL,
    ingested_at TEXT NOT NULL,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
    cache_creation_5m_tokens INTEGER NOT NULL DEFAULT 0,
    cache_creation_1h_tokens INTEGER NOT NULL DEFAULT 0,
    usage_model TEXT NOT NULL DEFAULT '',
    message_id TEXT NOT NULL DEFAULT '',
    input_token_semantics INTEGER NOT NULL DEFAULT 0,
    fresh_input_tokens INTEGER NOT NULL DEFAULT 0,
    real_total_tokens INTEGER NOT NULL DEFAULT 0,
    latency_ms INTEGER,
    first_token_ms INTEGER,
    duration_ms INTEGER,
    generation_duration_ms INTEGER,
    speed_tps REAL,
    timing_quality TEXT NOT NULL DEFAULT 'unknown',
    status_code INTEGER,
    error_message TEXT NOT NULL DEFAULT '',
    is_streaming INTEGER NOT NULL DEFAULT 0,
    cost_multiplier TEXT NOT NULL DEFAULT '1',
    pricing_version TEXT NOT NULL DEFAULT '',
    cost_usd TEXT NOT NULL DEFAULT '0',
    cost_cny TEXT NOT NULL DEFAULT '0',
    input_cost_usd TEXT NOT NULL DEFAULT '0',
    output_cost_usd TEXT NOT NULL DEFAULT '0',
    cache_read_cost_usd TEXT NOT NULL DEFAULT '0',
    cache_creation_cost_usd TEXT NOT NULL DEFAULT '0',
    input_cost_cny TEXT NOT NULL DEFAULT '0',
    output_cost_cny TEXT NOT NULL DEFAULT '0',
    cache_read_cost_cny TEXT NOT NULL DEFAULT '0',
    cache_creation_cost_cny TEXT NOT NULL DEFAULT '0',
    metadata TEXT NOT NULL DEFAULT '{}',
    UNIQUE (device_id, source, source_record_id)
);

CREATE INDEX IF NOT EXISTS idx_usage_occurred_at ON usage_events(occurred_at);
CREATE INDEX IF NOT EXISTS idx_usage_device ON usage_events(device_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_usage_app ON usage_events(source_app, occurred_at);
CREATE INDEX IF NOT EXISTS idx_usage_provider ON usage_events(provider_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_usage_model ON usage_events(model, occurred_at);

CREATE TABLE IF NOT EXISTS outbox (
    outbox_id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL,
    device_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    target TEXT NOT NULL DEFAULT 'upstream',
    payload TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    sent_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_outbox_pending ON outbox(status, created_at, outbox_id);

CREATE TABLE IF NOT EXISTS sync_state (
    device_id TEXT PRIMARY KEY,
    last_event_at TEXT,
    last_success_at TEXT,
    cursor TEXT,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS collector_cursors (
    source_id TEXT NOT NULL,
    file_path TEXT NOT NULL,
    offset INTEGER NOT NULL DEFAULT 0,
    modified_nanos INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    PRIMARY KEY (source_id, file_path)
);

CREATE TABLE IF NOT EXISTS usage_daily_rollups (
    date TEXT NOT NULL,
    device_id TEXT NOT NULL,
    app_type TEXT NOT NULL,
    provider_id TEXT NOT NULL DEFAULT '',
    provider_name TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL,
    request_model TEXT NOT NULL DEFAULT '',
    pricing_model TEXT NOT NULL DEFAULT '',
    request_count INTEGER NOT NULL DEFAULT 0,
    success_count INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
    total_cost_usd TEXT NOT NULL DEFAULT '0',
    total_cost_cny TEXT NOT NULL DEFAULT '0',
    avg_latency_ms INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (date, device_id, app_type, provider_id, model, request_model, pricing_model)
);
