-- Core schema. OAuth (fosite) tables are added in a later migration (phase 7).

CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    token_hash BYTEA PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE linkedin_accounts (
    id                    BIGSERIAL PRIMARY KEY,
    user_id               BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    identification_token  TEXT NOT NULL,          -- server-side only, never sent to FE
    status                TEXT NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending','connected','disconnected','error')),
    connected_at          TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON linkedin_accounts (user_id);

-- Weekday / working-hours rules and caps. Consumed by internal/schedule.
CREATE TABLE account_schedules (
    account_id        BIGINT PRIMARY KEY REFERENCES linkedin_accounts(id) ON DELETE CASCADE,
    timezone          TEXT NOT NULL DEFAULT 'UTC',
    work_days         SMALLINT[] NOT NULL DEFAULT '{1,2,3,4,5}',  -- 0=Sun .. 6=Sat
    start_minute      INT NOT NULL DEFAULT 570  CHECK (start_minute BETWEEN 0 AND 1439),   -- 09:30
    end_minute        INT NOT NULL DEFAULT 1050 CHECK (end_minute BETWEEN 1 AND 1440),     -- 17:30
    holidays          DATE[] NOT NULL DEFAULT '{}',
    daily_connect_cap INT NOT NULL DEFAULT 5,
    daily_message_cap INT NOT NULL DEFAULT 10,
    ramp_started_at   DATE NOT NULL DEFAULT CURRENT_DATE,
    CHECK (start_minute < end_minute)
);

CREATE TABLE leads (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    profile_url TEXT NOT NULL,
    name        TEXT NOT NULL DEFAULT '',
    headline    TEXT NOT NULL DEFAULT '',
    company     TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, profile_url)
);

CREATE TABLE campaigns (
    id          BIGSERIAL PRIMARY KEY,
    account_id  BIGINT NOT NULL REFERENCES linkedin_accounts(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    steps       JSONB NOT NULL,   -- [{type:"connect",note},{type:"message",body,delay_days},{type:"followup",body,delay_days}]
    daily_limit INT NOT NULL DEFAULT 10,
    status      TEXT NOT NULL DEFAULT 'draft'
                CHECK (status IN ('draft','running','paused','stopped','done')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON campaigns (account_id);

CREATE TABLE campaign_leads (
    campaign_id    BIGINT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    lead_id        BIGINT NOT NULL REFERENCES leads(id) ON DELETE CASCADE,
    state          TEXT NOT NULL DEFAULT 'queued'
                   CHECK (state IN ('queued','request_sent','connected','messaged',
                                    'followed_up','replied','done','failed')),
    current_step   INT NOT NULL DEFAULT 0,
    next_action_at TIMESTAMPTZ,   -- always a value produced by schedule.NextSlot
    last_error     TEXT,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (campaign_id, lead_id)
);
-- Worker scan: due, active leads.
CREATE INDEX campaign_leads_due ON campaign_leads (next_action_at)
    WHERE state NOT IN ('replied','done','failed');

CREATE TABLE messages (
    id         BIGSERIAL PRIMARY KEY,
    lead_id    BIGINT NOT NULL REFERENCES leads(id) ON DELETE CASCADE,
    direction  TEXT NOT NULL CHECK (direction IN ('out','in')),
    body       TEXT NOT NULL,
    at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON messages (lead_id, at);

-- Webhook dedup: insert with ON CONFLICT DO NOTHING; skip processing if 0 rows.
CREATE TABLE webhook_events (
    event_id    TEXT PRIMARY KEY,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
