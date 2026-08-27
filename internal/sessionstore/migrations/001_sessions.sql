CREATE TABLE IF NOT EXISTS xworkmate_session_namespaces (
  account_id text NOT NULL,
  namespace_id text NOT NULL,
  name text NOT NULL,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  PRIMARY KEY (account_id, namespace_id)
);

CREATE TABLE IF NOT EXISTS xworkmate_task_sessions (
  session_id uuid PRIMARY KEY,
  account_id text NOT NULL,
  namespace_id text NOT NULL,
  title text NOT NULL DEFAULT '',
  lifecycle_state text NOT NULL,
  snapshot_version bigint NOT NULL,
  last_event_seq bigint NOT NULL,
  context jsonb NOT NULL DEFAULT '{}',
  current_task_run_id uuid,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  FOREIGN KEY (account_id, namespace_id) REFERENCES xworkmate_session_namespaces(account_id, namespace_id)
);
CREATE INDEX IF NOT EXISTS xworkmate_task_sessions_account_namespace_updated_idx ON xworkmate_task_sessions(account_id, namespace_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS xworkmate_session_events (
  session_id uuid NOT NULL REFERENCES xworkmate_task_sessions(session_id) ON DELETE CASCADE,
  seq bigint NOT NULL,
  event_type text NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL,
  PRIMARY KEY (session_id, seq)
);

CREATE TABLE IF NOT EXISTS xworkmate_session_messages (
  message_id uuid PRIMARY KEY,
  session_id uuid NOT NULL REFERENCES xworkmate_task_sessions(session_id) ON DELETE CASCADE,
  task_run_id uuid NOT NULL,
  client_request_id text NOT NULL,
  role text NOT NULL,
  text text NOT NULL,
  created_at timestamptz NOT NULL,
  UNIQUE (session_id, client_request_id)
);

CREATE TABLE IF NOT EXISTS xworkmate_task_runs (
  run_id uuid PRIMARY KEY,
  session_id uuid NOT NULL REFERENCES xworkmate_task_sessions(session_id) ON DELETE CASCADE,
  state text NOT NULL,
  bridge_task_ref text NOT NULL DEFAULT '',
  priority integer NOT NULL DEFAULT 0,
  not_before timestamptz,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS xworkmate_task_runs_session_updated_idx ON xworkmate_task_runs(session_id, updated_at DESC);

DO $$ BEGIN
  ALTER TABLE xworkmate_task_sessions ADD CONSTRAINT xworkmate_task_sessions_current_run_fk FOREIGN KEY (current_task_run_id) REFERENCES xworkmate_task_runs(run_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
  ALTER TABLE xworkmate_session_messages ADD CONSTRAINT xworkmate_session_messages_task_run_fk FOREIGN KEY (task_run_id) REFERENCES xworkmate_task_runs(run_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
