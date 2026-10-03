-- 000006_create_audit_events.up.sql (02 §2.1; 05 §15.4; T-45). Append-only
-- audit log with a hash chain. Privileges:
--   app_rw        SELECT (INSERT only through its INHERIT membership in audit_writer)
--   audit_writer  INSERT
--   audit_chainer SELECT + UPDATE of the four chain columns only; the chain job
--                 runs SET LOCAL ROLE audit_chainer (app_rw is a NOINHERIT member)
-- audit_events_guard is the second layer: a chained row never changes, only
-- the chain columns of an unchained row may change, and rows are deleted only
-- by audit_purge_until() (000008).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE audit_events (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  seq            bigint GENERATED ALWAYS AS IDENTITY CONSTRAINT audit_events_seq_uq UNIQUE, -- insertion order (not the chain order)
  occurred_at    timestamptz NOT NULL DEFAULT now(),
  actor_kind     text NOT NULL CONSTRAINT audit_events_actor_kind_chk CHECK (actor_kind IN ('user','system','anonymous')),
  actor_user_id  uuid,                                         -- no FK: audit is independent and permanent
  actor_org_id   uuid,
  system_job     text,
  action         text NOT NULL CONSTRAINT audit_events_action_chk CHECK (action ~ '^[a-z_]+(\.[a-z_]+)+$'), -- = authz action name
  outcome        text NOT NULL CONSTRAINT audit_events_outcome_chk CHECK (outcome IN ('success','denied','failure')),
  org_id         uuid,                                         -- affected org (scope)
  resource_type  text,
  resource_id    uuid,
  reason         text CONSTRAINT audit_events_reason_chk CHECK (char_length(reason) <= 500), -- break-glass / export justification
  ip             inet,
  user_agent     text CONSTRAINT audit_events_user_agent_chk CHECK (char_length(user_agent) <= 400),
  request_id     text,
  metadata       jsonb NOT NULL DEFAULT '{}',                  -- no password/token/special-category VALUES
  chain_seq      bigint CONSTRAINT audit_events_chain_seq_uq UNIQUE, -- set by the chainer
  prev_hash      bytea CONSTRAINT audit_events_prev_hash_chk CHECK (octet_length(prev_hash) = 32),
  row_hash       bytea CONSTRAINT audit_events_row_hash_chk CHECK (octet_length(row_hash) = 32),
  chained_at     timestamptz,
  CONSTRAINT audit_events_chain_chk  CHECK ((chain_seq IS NULL) = (row_hash IS NULL)),
  CONSTRAINT audit_events_user_chk   CHECK (actor_kind <> 'user'   OR actor_user_id IS NOT NULL),
  CONSTRAINT audit_events_system_chk CHECK (actor_kind <> 'system' OR system_job IS NOT NULL)
);
CREATE INDEX audit_events_unchained_idx ON audit_events (seq) WHERE chain_seq IS NULL;
CREATE INDEX audit_events_actor_idx     ON audit_events (actor_user_id, occurred_at DESC) WHERE actor_user_id IS NOT NULL;
CREATE INDEX audit_events_resource_idx  ON audit_events (resource_type, resource_id, occurred_at DESC);
CREATE INDEX audit_events_org_idx       ON audit_events (org_id, occurred_at DESC) WHERE org_id IS NOT NULL;
CREATE INDEX audit_events_action_idx    ON audit_events (action, occurred_at);

CREATE FUNCTION public.audit_events_guard() RETURNS trigger
  LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF current_setting('koro.audit_purge', true) = 'on' THEN
      RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit_events rows are deleted only by audit_purge_until()'
      USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_OP = 'TRUNCATE' THEN
    RAISE EXCEPTION 'TRUNCATE on audit_events is not allowed'
      USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.chain_seq IS NOT NULL THEN
    RAISE EXCEPTION 'audit_events row % is already chained and cannot change', OLD.id
      USING ERRCODE = 'restrict_violation';
  END IF;
  IF (to_jsonb(NEW) - ARRAY['chain_seq','prev_hash','row_hash','chained_at'])
     IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['chain_seq','prev_hash','row_hash','chained_at']) THEN
    RAISE EXCEPTION 'only the hash chain columns of audit_events may be updated'
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER audit_events_guard BEFORE UPDATE OR DELETE ON audit_events
  FOR EACH ROW EXECUTE FUNCTION public.audit_events_guard();
CREATE TRIGGER audit_events_guard_truncate BEFORE TRUNCATE ON audit_events
  FOR EACH STATEMENT EXECUTE FUNCTION public.audit_events_guard();

GRANT SELECT ON audit_events TO app_rw;
GRANT INSERT ON audit_events TO audit_writer;
GRANT SELECT ON audit_events TO audit_chainer;
GRANT UPDATE (chain_seq, prev_hash, row_hash, chained_at) ON audit_events TO audit_chainer;
COMMIT;
