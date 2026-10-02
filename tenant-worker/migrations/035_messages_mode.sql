-- Voice/text response tailoring for mobile — a single mobile session serves
-- both typing and speaking, so unlike Discord voice (a structurally separate
-- platform/session, its prompt frozen once at session genesis in
-- sessions.system_prompt), mode has to be a per-message, per-turn decision,
-- not a per-session one.
--
-- NULL/absent means "text" (the existing default behavior for every
-- platform that never sends this — Discord, web, and mobile messages sent
-- before the client started setting it).
ALTER TABLE messages ADD COLUMN mode text;
