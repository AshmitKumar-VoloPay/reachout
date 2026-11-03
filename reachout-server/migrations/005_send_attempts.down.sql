DROP TABLE IF EXISTS send_attempts;
ALTER TABLE campaign_leads DROP COLUMN IF EXISTS retry_count;
ALTER TABLE account_schedules DROP COLUMN IF EXISTS next_send_not_before;
