DROP INDEX IF EXISTS one_pending_connection;
ALTER TABLE linkedin_accounts DROP CONSTRAINT IF EXISTS linkedin_accounts_status_check;
ALTER TABLE linkedin_accounts ADD CONSTRAINT linkedin_accounts_status_check
    CHECK (status IN ('pending','connected','disconnected','error'));
ALTER TABLE linkedin_accounts
    DROP COLUMN IF EXISTS status_detail,
    DROP COLUMN IF EXISTS reconnection_link,
    DROP COLUMN IF EXISTS known_account_ids,
    DROP COLUMN IF EXISTS connection_session_id,
    DROP COLUMN IF EXISTS avatar_url,
    DROP COLUMN IF EXISTS headline,
    DROP COLUMN IF EXISTS profile_url,
    DROP COLUMN IF EXISTS name,
    DROP COLUMN IF EXISTS linkedin_account_id;
-- identification_token NOT NULL is not restored: rows may now hold NULL.
