DROP INDEX IF EXISTS campaign_leads_lead;
ALTER TABLE campaign_leads DROP CONSTRAINT IF EXISTS campaign_leads_step_check;
ALTER TABLE campaign_leads
    DROP CONSTRAINT IF EXISTS campaign_leads_lead_owner_fk,
    DROP CONSTRAINT IF EXISTS campaign_leads_campaign_owner_fk,
    DROP COLUMN IF EXISTS user_id;
ALTER TABLE campaigns DROP CONSTRAINT IF EXISTS campaigns_account_owner_fk;
ALTER TABLE campaigns DROP CONSTRAINT IF EXISTS campaigns_id_user_key;
ALTER TABLE leads DROP CONSTRAINT IF EXISTS leads_id_user_key;
ALTER TABLE linkedin_accounts DROP CONSTRAINT IF EXISTS linkedin_accounts_id_user_key;
ALTER TABLE campaigns DROP CONSTRAINT IF EXISTS campaigns_daily_limit_check;
DROP INDEX IF EXISTS campaigns_user;
ALTER TABLE campaigns DROP COLUMN IF EXISTS started_at, DROP COLUMN IF EXISTS user_id;
