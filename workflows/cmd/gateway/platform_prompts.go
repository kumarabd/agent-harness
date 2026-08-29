package main

// platformSystemPrompts — docs/components/gateway/discord-voice.md's Notes
// Log, "responses aren't conversation-friendly" (real user report: TTS
// reading "asterisk asterisk" and emoji names aloud). A literal per-platform
// lookup, not a registry or a flag threaded through the reasoning path —
// same idiom turn.go's connectionDeliveryActivity already uses for exactly
// this shape of problem ("a literal lookup... adding a third is a one-line
// change, not a reason to build an abstraction for cases that don't exist
// yet"). A platform absent from this map (every platform today except
// discord-voice) gets no override at all: sessions.system_prompt stays
// unset, and ModelCall (activities/activities/llm.py) falls back to its own
// DEFAULT_SYSTEM_PROMPT exactly as it always has — this table is additive,
// never a behavior change for anything not explicitly listed here.
//
// Resolved and applied in exactly one place — submitMessageEvent's genesis
// INSERT (inbound.go), the one moment a session's platform is decided and
// will never change again. Nothing downstream (ModelCall, TurnWorkflow, the
// streaming machinery) has any platform-awareness added for this at all;
// they read sessions.system_prompt exactly as generically as before — the
// platform-specific decision lives here, once, expressed as data in a
// column that already existed and was simply never populated, not as a
// conditional anywhere in the reasoning loop.
var platformSystemPrompts = map[string]string{
	"discord-voice": voiceSystemPromptText,
}

// voiceSystemPromptText is a genuinely standalone prompt, not a diff against
// DEFAULT_SYSTEM_PROMPT — voice's own formatting constraints (no markdown,
// no emoji, spoken-form numbers) apply regardless of what the base framing
// says, so patching a shared default rather than writing a real standalone
// prompt was never going to fully fit. Tool availability itself is
// unaffected either way — TOOLS_SCHEMA is passed to every ModelCall
// regardless of which system prompt is active, so this only changes
// framing/register, never what the model can actually call.
//
// search_skills nudge added 2026-08-29, same day DEFAULT_SYSTEM_PROMPT
// itself got the equivalent sentence (llm.py) — a spoken request for a
// known, repeatable procedure ("walk me through the deploy process") is
// just as real a case here as it is in text, and this prompt shouldn't
// silently lag the shared default's own tool-routing guidance just because
// its formatting rules are platform-specific. Kept as a single short
// sentence, consistent with this prompt's own "keep responses reasonably
// brief" instruction to the model applying equally to the model's own
// instructions here.
//
// The final line is copied verbatim from DEFAULT_SYSTEM_PROMPT's own last
// sentence, not reworded — declare_next_step_hint being called every
// response is a real, functionally-required mechanism (model_registry's
// escalate-on-retry / tier hinting depends on it), not a style choice, so
// it has to survive this rewrite exactly. Matches
// llm.py's _NEXT_STEP_HINT_TOOL_NAME by literal string, same as
// DEFAULT_SYSTEM_PROMPT's own f-string does today — no cross-language
// constant sharing exists for this either way; re-verify this string if
// that Python constant's name ever changes.
const voiceSystemPromptText = `You are a helpful, friendly voice assistant. The user is speaking to you out loud, and your response will be read aloud by a text-to-speech system, not displayed as text — write accordingly:

- Never use markdown formatting: no asterisks, no bullet points, no headers, no bold or italics.
- Never use emoji.
- Write numbers, times, dates, and abbreviations the way you would actually say them out loud (for example, "three thirty," not "3:30").
- Keep responses conversational and reasonably brief — this is a spoken conversation, not a document. If you have several points to make, say them as connected sentences rather than a list.
- Sound natural and warm, the way a person would speak, not like a formal written answer.

If a request looks like a known, repeatable procedure rather than a one-off question, check search_skills first before improvising, and use get_skill to read a matching result's full guidance.

Every response, also call declare_next_step_hint alongside anything else you call, declaring what the next step needs.`
