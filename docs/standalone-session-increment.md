# Standalone session API (#38 / #35)

Bridge can operate independently of Accounts. A nonempty
`BRIDGE_ACCOUNTS_SESSION_API_URL` selects Accounts exclusively: invalid configuration,
upstream failure, or upstream authorization failure never selects local storage.
The proxy retains Accounts bearer validation upstream. With the URL absent, local
sessions require a bearer matching a configured Bridge static token; empty token
configuration denies access, even though older ACP routes permit development auth.
Accounts introspection is not consulted for this local path. Accounts session
storage and Accounts account/identity integration are separately optional: setting
an introspection URL does not select Accounts session storage or disable local
static bearer validation.

## Configuration

- `BRIDGE_SESSION_PG_URL`: PostgreSQL connection string, supplied at runtime.
- `BRIDGE_SESSION_DIR`: absolute private directory, permissions 0700; files use 0600.
- `BRIDGE_AUTH_TOKEN`: required local bearer (existing Bridge static token aliases
  also work). All accepted static bearers authenticate the same installation user.
- `BRIDGE_SESSION_OWNER_ID`: stable installation user identity, default
  `local-single-user`. Bearer values are never used as durable identity. Keep this
  value unchanged during token rotation. Changing it deliberately isolates history;
  it is an administrator setting, never a request header or client-supplied owner.
- `BRIDGE_ROUTER_TYPE=dsh` and `BRIDGE_DSH_HARNESS_URL`: select/configure the existing
  DSH executor. Model selection remains in the existing resolver/catalog.

The provider initializes lazily, creates the `bridge_local_sessions`,
`bridge_local_runs`, `bridge_local_events` and `bridge_local_cancel_requests` tables, and fails closed
on storage errors. Correct configuration and restart after initialization failure.
Use a dedicated database role with schema creation privileges for initialization.
Metadata includes session ID, stable owner identity hash, idempotency keys and
request hashes, run lifecycle, event sequence/type, body references and timestamps.
Titles, user messages, executor results, errors and event payloads are stored in
immutable private files, never as PostgreSQL transcript JSON or text columns.
Files and their containing directory are synced before the database reference
commits. Back up database and directory together; neither alone is sufficient.
No credentials files are read and driver errors are not returned or logged.

## Conversation contract

Namespaces, create/list, snapshot and ordered event replay use the existing `/api/v1`
routes and JSON field names. Create optionally accepts `clientRequestId`: exact
replays identify the same session; changed title returns 409. Listing is capped
at 100. Session creation emits no event; initial snapshot version is 1, sequence 0.
Authorization uses the configured installation owner, independently of optional
Accounts identity. The personal namespace includes `slug: personal` for App lookup.

`POST /api/v1/sessions/{id}/messages` accepts `clientRequestId`, `text` and optional
existing routing hints. It returns 202 with `sessionId`, `namespaceId`,
`snapshotVersion`, `event`, and `taskRun: {id, state}`. Exact retries return 200 and
the original immutable receipt without redispatch, even after restart. Changed
content under the same request ID returns 409. A different message while the
session has an active run returns 409. Different sessions can execute concurrently.

The input, initial events and receipt commit before dispatch. The execution
goroutine delegates to `s.orchestrator.Process` with `session.start` on the first
turn and `session.message` thereafter, preserving session/thread IDs. HTTP client
disconnect does not cancel accepted work. Execution is bounded to 30 minutes.
No local planner, role resolver, scheduler or second provider process pool is added.
Role routing continues to follow the existing orchestrator's rejection behavior;
Immediate runs may include `run.priority: 0`; other scheduling options return 422.
Use the configured DSH route for synchronous terminal results. Asynchronously
accepted results are not treated as successful completion.

App `foldBotTasks` events are `message.created`, `run.queued`, `run.running`,
`run.progressed` (`phase: started`), `assistant.message.created`, and terminal
`run.completed`, `run.failed` or `run.cancelled`. Every payload includes
`schemaVersion: 1` and `taskRunId`. User and assistant events carry `text`; failure
events carry `code` and `error`. Events replay in strictly increasing contiguous
`seq` order, with RFC3339 `createdAt`. `after_seq` and `limit` (1–500) paginate a
response capped at 4 MiB; `lastEventSeq` is the last returned cursor, not an unseen
future high watermark. Snapshots expose a consistent cursor and latest run state.

Cancellation persists `run.cancel_requested`, invokes the active context's cancel
function, and returns `contextCancelled: true` only after that context is cancelled.
`run.cancelled` is persisted only once the executor call returns. This acknowledges
Bridge execution-context cancellation, not proof that an independent remote
executor process has stopped. Duplicate cancel requests do not duplicate events;
reusing a cancellation request ID for another run returns 409.

Only one Bridge instance may admit local execution, enforced by a PostgreSQL
session advisory lock plus durable execution-hazard records. A background probe
cancels local contexts when the ownership connection is lost. That cancellation
cannot prove a remote executor has stopped, so takeover does not permit new
dispatch while hazards from a prior instance remain. After restart, persisted
queued/running/cancel-requested runs receive `run.failed` with
`BRIDGE_RESTART_INTERRUPTED`; they are never automatically reexecuted. Retrying
their original message returns its original receipt, not another execution.

A successful synchronous completion on the original ownership connection clears
its hazard. Failed, cancelled or interrupted dispatched work conservatively
retains it: that session cannot immediately start another run, and a successor
instance refuses dispatch until administrative recovery. Recovery requires stopping
all prior Bridge processes, independently checking/stopping their remote executor
work, and only then removing the specifically verified run IDs from
`bridge_local_execution_hazards` using privileged database access. Never clear all
records blindly. There is no bearer API or environment bypass. Reads remain
available; context cancellation or an HTTP success alone does not justify clearing
the gate. This deliberate availability tradeoff avoids claiming remote fencing
that the execution protocol does not provide.

The existing DSH adapter propagates transport/protocol errors instead of
manufacturing successful responses. No successful chat is manufactured when a
provider is unavailable or returns an error. Persistence failure after execution
records `RESULT_PERSIST_FAILED` where storage remains writable; a continuing
storage outage leaves the run for restart reconciliation, without executor retry.

Retries and failed database writes may leave unreferenced immutable files;
automatic garbage collection is deliberately absent. Parent directories must be
trusted and storage must not be modified by other processes. This increment does
not implement backup automation, multi-host file sharing, retention, account
migration, session-list pagination beyond 100, or App/device runtime acceptance.
This implementation has not been deployed or tested against a live model service.
Earlier unpublished foundation builds that used bearer hashes as owner identity
require an explicit metadata migration; this change does not silently relabel owners.

## Verification

`BRIDGE_TEST_PG_FIXTURE=1 go test -race ./internal/acp -run 'TestLocalSession|TestDSHFailures|TestConfiguredAccounts' -count=1`
starts and stops an isolated local PostgreSQL fixture, asserts PostgreSQL 17,
checks create replay/conflict, concurrent duplicate message acceptance, one-time
dispatch, continuation, success/failure, cancellation observed by the executor,
no terminal cancellation before executor return, event replay/pagination,
restart interruption without reexecution, stable ownership across bearer changes,
cross-owner denial, exclusive database ownership, absence of transcript bodies in
PostgreSQL metadata, Accounts fail-closed selection and real DSH HTTP adapter
success/error mapping (mocked executor responses, not live model acceptance).
The fixture needs local `initdb` / `pg_ctl`; it never reads existing credentials.
Run `go test ./...`, `go vet ./...`, and `go build ./...` for repository checks.

The dedicated `Standalone session integration` CI workflow installs PostgreSQL 17
and explicitly enables this fixture. Missing binaries or the wrong server version
fail the check rather than skipping it. Review scope: Bridge #38 / #35 only;
deployment and credential rotation are separate acceptance steps.
