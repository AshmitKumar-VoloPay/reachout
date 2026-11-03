-- Campaigns get a direct owner, and the database itself now refuses cross-user links:
-- a campaign's account, and every lead in a campaign, must belong to the campaign's user.

ALTER TABLE campaigns
    ADD COLUMN user_id    BIGINT REFERENCES users(id) ON DELETE CASCADE,
    ADD COLUMN started_at TIMESTAMPTZ;
UPDATE campaigns c SET user_id = a.user_id FROM linkedin_accounts a WHERE a.id = c.account_id;
ALTER TABLE campaigns ALTER COLUMN user_id SET NOT NULL;
CREATE INDEX campaigns_user ON campaigns (user_id, id DESC);
ALTER TABLE campaigns ADD CONSTRAINT campaigns_daily_limit_check CHECK (daily_limit BETWEEN 1 AND 30);

ALTER TABLE linkedin_accounts ADD CONSTRAINT linkedin_accounts_id_user_key UNIQUE (id, user_id);
ALTER TABLE leads             ADD CONSTRAINT leads_id_user_key             UNIQUE (id, user_id);
ALTER TABLE campaigns         ADD CONSTRAINT campaigns_id_user_key         UNIQUE (id, user_id);

ALTER TABLE campaigns ADD CONSTRAINT campaigns_account_owner_fk
    FOREIGN KEY (account_id, user_id) REFERENCES linkedin_accounts (id, user_id) ON DELETE CASCADE;

ALTER TABLE campaign_leads ADD COLUMN user_id BIGINT;
UPDATE campaign_leads cl SET user_id = c.user_id FROM campaigns c WHERE c.id = cl.campaign_id;
ALTER TABLE campaign_leads ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE campaign_leads
    ADD CONSTRAINT campaign_leads_campaign_owner_fk FOREIGN KEY (campaign_id, user_id) REFERENCES campaigns (id, user_id) ON DELETE CASCADE,
    ADD CONSTRAINT campaign_leads_lead_owner_fk     FOREIGN KEY (lead_id, user_id)     REFERENCES leads (id, user_id)     ON DELETE CASCADE;

-- current_step = furthest stage reached, never decreases:
-- 0 queued, 1 request sent, 2 connected, 3 messaged, 4 followed up.
ALTER TABLE campaign_leads ADD CONSTRAINT campaign_leads_step_check CHECK (current_step BETWEEN 0 AND 4);
CREATE INDEX campaign_leads_lead ON campaign_leads (lead_id);
