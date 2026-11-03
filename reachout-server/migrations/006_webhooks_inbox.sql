-- Webhook ingestion, inbound messages, and enabling Linked API's event feeds.

-- Dedupe rows now also say whether the event was fully handled, so an event whose handling
-- failed is processed again when Linked API retries it, instead of being lost.
ALTER TABLE webhook_events
    ADD COLUMN type         TEXT NOT NULL DEFAULT '',
    ADD COLUMN processed_at TIMESTAMPTZ,
    ADD COLUMN detail       TEXT NOT NULL DEFAULT '';

-- Messages: inbound ones carry LinkedIn's message id, so the same message arriving in two
-- different events is stored once; read_at drives the unread count in the inbox.
ALTER TABLE messages
    ADD COLUMN external_id TEXT,
    ADD COLUMN thread_id   TEXT,
    ADD COLUMN read_at     TIMESTAMPTZ;
CREATE UNIQUE INDEX messages_external ON messages (lead_id, external_id) WHERE external_id IS NOT NULL;

-- Events identify people by public URL, hashed URL or URN. Remember the other forms once
-- seen, so later events that carry only one of them still find the lead.
ALTER TABLE leads
    ADD COLUMN linkedin_urn TEXT,
    ADD COLUMN hashed_url   TEXT;
CREATE INDEX leads_urn    ON leads (user_id, linkedin_urn) WHERE linkedin_urn IS NOT NULL;
CREATE INDEX leads_hashed ON leads (user_id, hashed_url)   WHERE hashed_url IS NOT NULL;

-- status_event_at: time of the newest account status event applied (events can arrive out
-- of order, the newest wins). *_synced_at: Linked API only sends network and inbox events
-- after st.syncNetwork / st.syncInbox have run once for the account.
ALTER TABLE linkedin_accounts
    ADD COLUMN status_event_at   TIMESTAMPTZ,
    ADD COLUMN network_synced_at TIMESTAMPTZ,
    ADD COLUMN inbox_synced_at   TIMESTAMPTZ;

ALTER TABLE send_attempts DROP CONSTRAINT send_attempts_kind_check;
ALTER TABLE send_attempts ADD CONSTRAINT send_attempts_kind_check
    CHECK (kind IN ('connect','message','followup','sync_network','sync_inbox'));

-- Failed webhook deliveries we asked Linked API to send again, so each is replayed a bounded number of times.
CREATE TABLE webhook_replays (
    delivery_id      TEXT PRIMARY KEY,
    replays          INT NOT NULL DEFAULT 0,
    last_replayed_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX webhook_events_received ON webhook_events (received_at);
