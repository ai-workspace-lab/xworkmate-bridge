# ADR: Bridge-owned shared task-session persistence

Status: accepted

## Decision

`xworkmate-bridge` is the system of record for cross-terminal task sessions.
Portal and desktop/mobile clients consume the same snapshot plus ordered-event
contract; Portal remains a display/proxy layer. PostgreSQL stores account-
scoped session metadata, allowlisted message context, ordered events, and the
current task-run state.

Accounts remains the identity authority. Credential introspection provides the
immutable `accountId`; Bridge ignores any account identity claimed by a client
request. Every repository operation includes `account_id`, and an ownership
mismatch is reported as not found.

## Event and concurrency model

Each session owns a monotonically increasing `seq`. Message append locks the
session row and atomically inserts `message.created` and `run.queued`, advances
the replay watermark, updates context, and creates the queued run.
`clientRequestId` is unique within a session, so a retry returns the original
event/run. ACP completion produces an allowlisted `run.state_changed` event and
preserves the provider's opaque task identifier as `bridgeTaskRef`.

Clients fetch a snapshot and then request events strictly after their known
watermark. Reconnecting clients converge without relying on the Bridge process
that originally accepted the message.

## Artifact boundary

The persistence API cannot represent artifact binaries, inline content,
attachments, file paths, URLs, base64 payloads, working directories, artifact
scopes, tool logs, or generic provider output. Artifact discovery and download
remain in the existing Bridge artifact domain and correlate only through the
task run ID and opaque `bridgeTaskRef`.

## Deployment dependency

`BRIDGE_ACCOUNTS_INTROSPECTION_URL` must return `accountId` for session API
requests, and `XWORKMATE_SESSION_DATABASE_URL` must point to PostgreSQL.
Existing ACP authorization continues accepting the legacy `{ "active": true }`
response, but shared-session routes fail closed until a principal is available.
