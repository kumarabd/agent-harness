-- lcm.assemble (docs/components/context-slot.md) reconstructs the verbatim
-- window on every real ModelCall: for each assistant message in the window it
-- looks up that message's tool_calls. There was no index on
-- tool_calls(message_id) — the FK to messages(message_id) does not create one
-- — so every one of those lookups sequentially scanned the whole tool_calls
-- table. assembly.py now batches them into a single
-- `WHERE message_id = ANY(...)`, which this index turns into an index scan.
CREATE INDEX tool_calls_message_id_idx ON tool_calls (message_id);
