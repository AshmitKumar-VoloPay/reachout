-- Connected LinkedIn accounts: a row exists from the moment a connection session starts
-- (status 'pending') and is filled in when Linked API reports the new account.

ALTER TABLE linkedin_accounts
    ALTER COLUMN identification_token DROP NOT NULL,       -- encrypted; null until connected
    ADD COLUMN linkedin_account_id   TEXT UNIQUE,          -- Linked API's account id
    ADD COLUMN name                  TEXT NOT NULL DEFAULT '',
    ADD COLUMN profile_url           TEXT NOT NULL DEFAULT '',
    ADD COLUMN headline              TEXT NOT NULL DEFAULT '',
    ADD COLUMN avatar_url            TEXT NOT NULL DEFAULT '',
    ADD COLUMN connection_session_id TEXT,
    ADD COLUMN known_account_ids     TEXT[] NOT NULL DEFAULT '{}',  -- Linked API accounts that existed before this session
    ADD COLUMN reconnection_link     TEXT,
    ADD COLUMN status_detail         TEXT NOT NULL DEFAULT '';

ALTER TABLE linkedin_accounts DROP CONSTRAINT linkedin_accounts_status_check;
ALTER TABLE linkedin_accounts ADD CONSTRAINT linkedin_accounts_status_check
    CHECK (status IN ('pending','connected','frozen','reconnection_required','disconnected','error'));

-- Linked API gives no link between a connection session and the account it creates, so we
-- only allow one connection in flight at a time across all users. That makes "the one new
-- account" unambiguous and stops one user from being handed another user's LinkedIn account.
CREATE UNIQUE INDEX one_pending_connection ON linkedin_accounts ((true))
    WHERE status = 'pending';
