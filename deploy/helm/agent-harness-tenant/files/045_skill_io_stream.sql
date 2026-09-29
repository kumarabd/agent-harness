-- Skill iterations appear as tool activity in the existing Web/mobile stream.
ALTER TABLE skill_io ADD COLUMN completed_at timestamptz;

CREATE OR REPLACE FUNCTION _realtime_notify_skill_io() RETURNS trigger AS $$
DECLARE
  turn_id text;
BEGIN
  SELECT parent_id INTO turn_id FROM tool_calls WHERE tool_call_id = NEW.step_id;
  IF turn_id IS NOT NULL THEN
    PERFORM _mobile_notify_for_turn(turn_id);
  END IF;
  RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER realtime_notify_skill_io
  AFTER INSERT OR UPDATE OF response, completed_at ON skill_io
  FOR EACH ROW EXECUTE FUNCTION _realtime_notify_skill_io();
