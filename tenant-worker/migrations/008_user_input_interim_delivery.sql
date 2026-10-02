-- docs/components/user-input.md — "Mid-turn interim delivery" (push half,
-- A+B). A pending request's prompt now gets actively pushed to connection-
-- based platforms (Discord text/voice) via a new per-platform interim-
-- delivery activity, not just logged. That activity needs its own
-- idempotency marker, independent of user_input_requests.status: a Temporal
-- retry of the dispatching ExecuteActivity call must not re-send the prompt
-- a second time, but "was the prompt pushed" is a different question from
-- "has the human answered yet" (status), so this is a separate column, not
-- inferred from status.
ALTER TABLE user_input_requests ADD COLUMN prompt_delivered_at timestamptz;
