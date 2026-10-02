-- docs/components/tool-registry.md, "Resolved: Three-Layer Tool Taxonomy &
-- Per-Task Resolution" — a per-task resolved tool (from ToolDiscover) is
-- offered to the model under its own name (e.g. "weather_lookup"), not
-- call_tool, so TOOL_REGISTRY has no handler for it. These two nullable
-- columns carry the {server, tool} identity ModelCall resolved at mint time
-- (from the Capability it built off turn_retrieval), so ToolCall can route the
-- call through the internal call_tool proxy instead of a TOOL_REGISTRY lookup.
-- NULL for every ordinary tool call (shell_exec, call_tool itself, memory_*,
-- lcm_*, intentions, subagent dispatch) — set only for a resolved dispatch.

ALTER TABLE tool_calls ADD COLUMN resolved_server TEXT;
ALTER TABLE tool_calls ADD COLUMN resolved_tool TEXT;
