-- Mobile gateway (docs/components/gateway/mobile.md) — the WebSocket stream.
--
-- 1. messages.client_msg_id: the client-generated id a mobile device stamps on
--    a message it sends, threaded through the signal payload so it lands here.
--    The device's own optimistic echo dedupes against this when the message
--    comes back over the fan-out stream (other devices in the same session
--    also receive it). Nullable — every message this process synthesizes
--    (assistant/tool rows, proactive seeds, subagent kickoffs) leaves it NULL.
ALTER TABLE messages ADD COLUMN client_msg_id text;

-- 2. NOTIFY on the three tables a mobile stream tails. The payload is the
--    top-level session_key; the gateway holds one LISTEN per replica and, on
--    each notification, re-reads that session's tail from its own per-session
--    cursor and pushes to its local sockets. A subagent turn (parent_type
--    'turn') has no session parent here, so it notifies nothing — mobile does
--    not stream subagent activity.
CREATE OR REPLACE FUNCTION _mobile_notify_for_turn(p_turn_id text) RETURNS void AS $$
DECLARE
  sk text;
BEGIN
  SELECT parent_id INTO sk FROM turns WHERE turn_id = p_turn_id AND parent_type = 'session';
  IF sk IS NOT NULL THEN
    PERFORM pg_notify('mobile_stream', sk);
  END IF;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION _mobile_notify_messages() RETURNS trigger AS $$
BEGIN
  PERFORM _mobile_notify_for_turn(NEW.parent_id);
  RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION _mobile_notify_turn_id() RETURNS trigger AS $$
BEGIN
  PERFORM _mobile_notify_for_turn(NEW.turn_id);
  RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER mobile_notify_messages       AFTER INSERT ON messages          FOR EACH ROW EXECUTE FUNCTION _mobile_notify_messages();
CREATE TRIGGER mobile_notify_turn_deliveries AFTER INSERT ON turn_deliveries  FOR EACH ROW EXECUTE FUNCTION _mobile_notify_turn_id();
CREATE TRIGGER mobile_notify_status_pings   AFTER INSERT ON turn_status_pings FOR EACH ROW EXECUTE FUNCTION _mobile_notify_turn_id();

-- Also fire when a turn flips to a terminal status, so the stream can emit
-- turn_end without waiting for the next message write.
CREATE OR REPLACE FUNCTION _mobile_notify_turn_status() RETURNS trigger AS $$
BEGIN
  IF NEW.status IS DISTINCT FROM OLD.status AND NEW.parent_type = 'session' THEN
    PERFORM pg_notify('mobile_stream', NEW.parent_id);
  END IF;
  RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER mobile_notify_turn_status AFTER UPDATE OF status ON turns FOR EACH ROW EXECUTE FUNCTION _mobile_notify_turn_status();
