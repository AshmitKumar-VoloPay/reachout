# Linked API contract (as read from linkedapi.io/docs, 2026-11-03)

Source pages: /docs, /docs/webhooks, /docs/webhook-signature-verification, /docs/webhook-event-types,
/docs/admin-overview, admin-accounts, admin-connection-sessions, admin-limits, admin-webhooks,
/docs/executing-workflows, /docs/working-hours, /docs/action-st-*.
Anything marked UNVERIFIED was not on a page I could read; confirm before coding against it.
No secrets belong in this file.

## Two APIs

| | Admin API | Account (workflow) API |
|---|---|---|
| Base | `https://api.linkedapi.io/admin/<resource>.<action>` (POST, JSON) | `https://api.linkedapi.io/workflows` |
| Headers | `linked-api-token`, `Content-Type: application/json` | `linked-api-token` **and** `identification-token` |
| Style | synchronous | asynchronous: start, then poll or webhook |

Envelope: success `{"success":true,"result":{...}}`; error `{"success":false,"error":{"type","message"}}`.
Admin errors: `linkedApiTokenRequired` 401, `invalidLinkedApiToken` 401, `accountNotFound` 401,
`sessionNotFound` 404, `tooManyRequests` 429 (**100 requests / 60 s** on the admin API).

## Connect flow (open question 2: hosted)

1. `POST /admin/accounts.createConnectionSession` (body optional, may carry a `completion` object)
   returns `{sessionId, connectionLink}`. `connectionLink` is `https://app.linkedapi.io/connection/<id>`.
   Errors: `subscriptionRequired`, `noAvailableSeats`, `dailyConnectionAttemptsExceeded`, `invalidRequestPayload`.
2. The user opens the link in a browser and logs into LinkedIn there. This is a Linked API hosted page, so the
   frontend only needs a "Connect LinkedIn" button that opens the link and then polls our backend.
3. `POST /admin/accounts.getConnectionSession {sessionId}` returns `session.status`:
   `pending, preparing, serving, streaming, success, expired, error, cancelled`. Session lifetime: UNVERIFIED.
4. On `success`, `POST /admin/accounts.getAll` (no body) returns `accounts[]` and `pendingConnectionSessions[]`.
   Account fields: `id` (UUID), `name`, `url`, `avatarUrl`, `headline`, `countryCode`, **`identificationToken`**,
   `status` (`active | frozen | reconnection_required`), `connectedAt`, and, when reconnecting,
   `reconnectionSessionId`, `reconnectionLink`.
   There is no field that links a session to the new account, so match by diffing `accounts` before and after
   (store the known account IDs when creating the session). UNVERIFIED whether `completion` can carry a reference.
5. Reconnect: `accounts.createReconnectionSession {accountId}` returns `{reconnectionSessionId, reconnectionLink}`
   (account must be `reconnection_required`).
6. Others: `accounts.cancelConnectionSession`, `accounts.disconnect`, `accounts.regenerateIdentificationToken
   {accountId}` returns `{token}` (old token dies immediately).

## Running a workflow

- Start: `POST /workflows` with `{"actionType":"st.<name>", ...params}`; returns
  `{workflowId, workflowStatus: pending|running, pendingReason: queued|outsideWorkingHours|null, message}`.
- Status: `GET /workflows/{workflowId}`; `workflowStatus` is `pending | running | completed | failed`.
  Completed: `completion: {actionType, success, data, error?}`. Failed: `failure: {reason, message}`.
- Cancel: `DELETE /workflows/{workflowId}`.
- One workflow at a time per account. A simple one takes about 20 s, complex ones minutes. Each account has its own queue.
- Start errors: `invalidWorkflow`, `subscriptionRequired`, `plusPlanRequired`, `linkedinAccountSignedOut`, `outsideWorkingHours`.
- Failure reasons: `linkedinAccountSignedOut`, `languageNotSupported` (account language must be en, de, es, fr or pt).
- Action error `limitExceeded` appears in `completion.error.type`.

## Actions we need

| Purpose | actionType | Params | Result and errors |
|---|---|---|---|
| Search people | `st.searchPeople` | `term` (1-50 chars) or `customSearchUrl` (a pasted LinkedIn people-search URL, overrides term/filter), `filter{}`, `limit` (default 10, max 1000) | items: `name, urn, publicUrl, headline, location, avatarUrl, viewerState.connectionDegree`. **No company field, no `hashedUrl`.** Errors `searchingNotAllowed`, `filterNotApplied`. |
| Connection request | `st.sendConnectionRequest` | `personUrl`, `note` (max 300 chars, 200 on free LinkedIn), `email?` | success has no data. Errors: `personNotFound, selfProfileNotAllowed, alreadyPending, alreadyConnected, invitationAlreadyReceived, emailRequired, noteTooLong, noteLimitExceeded, requestNotAllowed`. Sending limits surface as `requestNotAllowed`, not `limitExceeded`. |
| Connection status | `st.checkConnectionStatus` | `personUrl` | `data.connectionStatus`: `connected | pending | notConnected` |
| Message | `st.sendMessage` | `personUrl` + `text`, or `threadId` + `text` | success only |
| Enable inbox events | `st.syncInbox` | none (run once per account) | only messages after enabling are captured |
| Enable network events | `st.syncNetwork` | none (run once per account) | only changes after enabling; baseline produces no events |
| Pending requests | `st.retrievePendingRequests` | UNVERIFIED | fallback for acceptance polling |
| Person profile | `st.openPersonPage` | UNVERIFIED | needed if we want company per lead |

## Webhooks (open question 3)

- One active webhook per Linked API client (not per account). Register with `webhook.set {url (public HTTPS),
  payloadMode: fat|thin, signingEnabled, headers, events}`; also `webhook.get/delete/setEvents/setSigning/
  revealSecret/rotateSecret/setHeader(s)/deliveries/replayDelivery/sendTest`. The URL cannot be edited; delete and recreate.
- Envelope: `{ "id", "type", "createdAt", "data": {...} }`. `id` is stable across retries: **dedupe on it**.
- Delivery is at-least-once, order is best-effort. Retries on non-2xx or timeout: exponential backoff, 8 attempts. Reply 2xx fast.
- Events we use:
  - `workflow.completed` data `{workflowId, accountId, status, result}` (`result` omitted in `thin` mode)
  - `network.connectionAccepted` data `{accountId, personUrn, personUrl, detectedAt}`
  - `inbox.messageReceived` / `inbox.messageSent` data `{accountId, type: st|nv, threadId, personUrn, personHashedUrl,
    personPublicUrl|null, messageId, sender: us|them, text, time}`. `messageSent` also fires for API-sent messages, so filter on `sender`.
  - `account.reconnectionRequired | active | frozen | deleted` data `{accountId, status}`
  - `webhook.test`
- Every event carries `data.accountId`, which is the account `id` from `accounts.getAll`. Use it to map events to our `linkedin_accounts` row; drop unmapped events.
- Signature (off by default; enable with `signingEnabled`):
  - headers `linked-api-signature` (64 lowercase hex) and `linked-api-signature-timestamp` (unix seconds)
  - signed string = `<timestamp>` + `.` + raw body bytes; HMAC-SHA256; secret used as literal UTF-8 text (do not hex-decode)
  - accept timestamps within 300 s either way; constant-time compare; verify the raw bytes before parsing
  - test vector: secret = bytes 0x00..0x1f as hex text `000102...1f`, timestamp `1700000000`, body
    `{"id":"event-1","type":"webhook.test","data":{"message":"Hello"}}` gives
    `8f93c19766fe42f27da56fcb5ae11a909c406683ccc0766bd29edc52c71fd209`

## Limits and working hours (open question 4)

- Limits: `limits.get/getDefaults/getUsage/set/delete/resetToDefaults`, keyed by `accountId`. Categories include
  `stConnectionRequests`, `stMessages`, `stSearchQueries`, `stPersonProfileViews`. Periods `daily|weekly|monthly`.
  Entry `{category, period, maxValue, isEnabled}`. Illustrative defaults: connection requests 10/day, messages 50/day.
- Working hours exist per account but are configured **only in the Linked API dashboard (no API)**. Policies for
  requests outside the window: queue (default for new accounts), reject (`outsideWorkingHours`), or run anyway.
  A parked workflow is shown with `pendingReason: outsideWorkingHours` and fails with `workingHoursWaitExpired` after 7 days.
  An account with no window is never gated.
- Consequence: our schedule is the source of truth for when we submit. We only submit inside our window, so Linked
  API's own window should be wider than ours or unset. Our caps should be at or below the limits we set through `limits.set`.

## Seats and resale (open question 5)

- `subscription.getStatus / getSeats / setSeats {quantity, billingPeriod, seatType}`. One connected account uses one seat
  (`noAvailableSeats` otherwise). The admin token is a single product-level token; each end-user account has its own
  `identificationToken`. Whether reselling seats to our own customers is permitted: UNRESOLVED, check their terms before launch.

## Changes this implies for the Reachout plan

1. Acceptance and replies can arrive by webhook (`network.connectionAccepted`, `inbox.messageReceived`) once
   `st.syncNetwork` and `st.syncInbox` have been run once per account. The 24/7 poller becomes a fallback and reconciler,
   not the primary path. Polling endpoint paths are UNVERIFIED.
2. Lead `company` is not in search results. Leave it blank at import or enrich later with `st.openPersonPage` (UNVERIFIED payload).
3. Treat both `limitExceeded` (action error) and `requestNotAllowed` / `noteLimitExceeded` (connection request) as "limit hit":
   reschedule with `NextSlot`, do not fail the lead.
4. Webhook endpoint is global: one URL, many accounts, mapped by `data.accountId`.
5. The connect screen is small: open `connectionLink`, poll our API until the account appears.
