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
// The "say the answer out loud after a tool / finished task" bullet was
// added 2026-08-29 (docs/components/gateway/discord-voice.md's Notes Log):
// the first rewrite of this prompt dropped DEFAULT_SYSTEM_PROMPT's own
// "After using a tool, summarize the result in plain text for the user"
// sentence entirely, leaving voice MORE exposed to future-work.md §4 (the
// model ending a turn with declare_next_step_hint only, no real content)
// than text — and deliver_voice.go's own content=="" path then no-ops
// silently, so a tool-calling voice turn could finish having spoken nothing
// but a filler phrase. This is the DEFAULT prompt's instruction restated in
// spoken-conversation terms, not a new capability.
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
- After you use a tool or finish a task, always say the answer or outcome out loud in a sentence or two — tell the user what you found or what you did. Never end your turn silently: if you have a result, speak it.

Every response, also call declare_next_step_hint alongside anything else you call, declaring what the next step needs.`
