-- Worker bookkeeping. Every LinkedIn action the worker takes is recorded here before it is
-- sent, so a crash can never lead to a blind re-send, and daily caps are counted from real history.

ALTER TABLE account_schedules ADD COLUMN next_send_not_before TIMESTAMPTZ;  -- spacing / jitter between sends
ALTER TABLE campaign_leads    ADD COLUMN retry_count INT NOT NULL DEFAULT 0;  -- times LinkedIn pushed a send back

CREATE TABLE send_attempts (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id  BIGINT NOT NULL REFERENCES linkedin_accounts(id) ON DELETE CASCADE,
    -- campaign_id and lead_id are plain numbers on purpose: deleting a campaign or lead must
    -- not erase today's send history, or deleting and re-adding leads would dodge the daily caps.
    campaign_id BIGINT NOT NULL,
    lead_id     BIGINT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('connect','message','followup')),
    -- starting:  recorded, Linked API not called yet (or the call's outcome is not known)
    -- submitted: workflow accepted, waiting for its result
    -- succeeded | failed: final. unknown: may or may not have been sent; never retried automatically.
    status      TEXT NOT NULL CHECK (status IN ('starting','submitted','succeeded','failed','unknown')),
    workflow_id TEXT,
    detail      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ
);

-- One LinkedIn task at a time per account, enforced by the database so two worker
-- processes can never both start work for the same account.
CREATE UNIQUE INDEX one_open_attempt_per_account ON send_attempts (account_id) WHERE status IN ('starting','submitted');
CREATE INDEX send_attempts_account_day  ON send_attempts (account_id, created_at);
CREATE INDEX send_attempts_campaign_day ON send_attempts (campaign_id, kind, created_at);
