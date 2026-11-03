-- Search imports run as a Linked API workflow that takes seconds to minutes, so each
-- one is a row the browser polls. The poll advances it (no background worker needed yet).

CREATE TABLE lead_imports (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id  BIGINT NOT NULL REFERENCES linkedin_accounts(id) ON DELETE CASCADE,
    search_url  TEXT NOT NULL,
    requested   INT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running','completed','failed')),
    workflow_id TEXT,
    found       INT NOT NULL DEFAULT 0,   -- people returned by the search
    imported    INT NOT NULL DEFAULT 0,   -- new leads created
    duplicates  INT NOT NULL DEFAULT 0,   -- already in the user's leads
    invalid     INT NOT NULL DEFAULT 0,   -- results without a usable profile URL
    over_cap    INT NOT NULL DEFAULT 0,   -- skipped: per-user lead limit reached
    message     TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);
CREATE INDEX lead_imports_user ON lead_imports (user_id, id DESC);

-- An account runs one Linked API workflow at a time, so allow one running import per account.
CREATE UNIQUE INDEX one_running_import_per_account ON lead_imports (account_id) WHERE status = 'running';

-- Search and paging on the leads table.
CREATE INDEX leads_user_id_id ON leads (user_id, id DESC);
