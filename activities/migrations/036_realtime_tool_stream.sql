-- Realtime tool/progress presentation for capability-aware clients.
-- Existing mobile clients do not opt into tool_call frames, but the shared
-- gateway still needs a wake hint whenever durable tool state changes.
CREATE OR REPLACE FUNCTION _realtime_notify_tool_call() RETURNS trigger AS $$
BEGIN
  PERFORM _mobile_notify_for_turn(NEW.parent_id);
  RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER realtime_notify_tool_calls
  AFTER INSERT OR UPDATE OF status, result, reason, partial_output, completed_at
  ON tool_calls
  FOR EACH ROW EXECUTE FUNCTION _realtime_notify_tool_call();

