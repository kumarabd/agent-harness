"""skills.journaling — docs/05-architecture-domain-control-loops.md.

Selected by model_call.py whenever this turn's own ModelCallInput.mode ==
"journaling" (set by workflow/mode_journaling.go's own runTurn(..., mode)
call — a real top-level turn, not a scoped reasoning bridge) — replacing
2026-09-27's earlier RunReasoningTurn-based design entirely, after a real
production incident: that design's reasoning turn returned "ok" having
made zero tool calls at all, and the skill told the user "Saved" having
written nothing.

A genuinely standalone prompt, not a diff against llm.DEFAULT_SYSTEM_PROMPT
— same reasoning llm.VOICE_SYSTEM_PROMPT's own comment already gives.
Critically, this does NOT mention "registered skills" the way the default
prompt does — that exact framing is what caused the original incident's
sibling bug (a scoped reasoning turn recursively calling the very skill it
was already executing, because its system prompt told it "there's a
registered skill for this, call it," with no awareness it WAS that skill's
own internal work). There is no "journaling" tool offered here at all now —
only switch_mode, which is how this mode was entered and how it's left.

The final report_status sentence must stay in step with every other system
prompt's own equivalent (llm.DEFAULT_SYSTEM_PROMPT's own comment) — it's
how the model authors turn-pipeline.md's status/next_step output, not a
style choice. "report_status" is a literal here, not imported from
llm.py — this package deliberately has zero dependency on llm.py (see
base.py's own comment); duplicating this one stable literal is the same
convention providers/base.py already uses for the same reason.

2026-09-28: steps 3-6 were tightened after a real recurring incident — a
fresh dated child page kept getting created on separate journaling
sessions for what should have been the same day. Root-caused to two
compounding gaps, not the model lying about a successful write: (1) the
original wording never specified HOW to find an existing dated page, and
the model's own chosen method was a plain, unscoped workspace-wide Notion
search — ranked/indexed independently of "My Diary"'s actual page tree,
so a small recently-created child page can rank low or lag the search
index and simply not surface; (2) a real observed artifact — a fetched
page titled "📅 📅 2026-09-28" (a doubled emoji) — showed the title
format itself drifting across separate sessions, so even a would-be
exact-title search from a later session wouldn't match an earlier
session's differently-formatted title. Fixed by name-checking the actual
Notion tool schemas rather than assuming: notion-search's own `page_url`
parameter ("restrict keyword search to a page and its descendants") is a
documented, reliable way to check "does today's page exist under My
Diary specifically" — instructed explicitly, replacing the model's own
prior unscoped-search habit — with tolerant substring matching on the
date so an already-drifted existing title still gets recognized, plus a
pinned canonical title format for future creations, `allow_async=false`
on creation (the default is async, which could otherwise race the
mandatory fetch-back verification against a still-empty page), and an
explicit insert-not-replace instruction for appending to an existing
page. Deliberately still model-driven, not a native Go/Python activity —
considered and declined for now: the model retains judgment over the
actual Notion tool calls, this only tightens which calls/parameters it's
told to use.
"""

from __future__ import annotations

from .base import SkillMode, register

DESCRIPTION = (
    "a dedicated journaling session: the user is recording a diary entry (explicitly asking "
    "to, or you judging a thought worth preserving and confirming that with them). Switch to "
    "it as soon as that's decided, before doing any of the actual recording work — the mode's "
    "own curated instructions take over from there."
)

SYSTEM_PROMPT = (
    "You are in a dedicated journaling mode — the user is actively recording a diary entry, and "
    "this conversation is scoped to that single activity until it's genuinely finished. You have "
    "direct shell access (shell_exec) and your usual tools (discover_tools/call_tool for Notion, "
    "recall, ask_user), the same as always.\n\n"
    "RECORDING AN ENTRY.\n"
    "1. Read the entry back to the user in your own words (or verbatim, if that's what preserves "
    "their voice) and confirm via ask_user before writing anything — never write on a first pass "
    "without an explicit yes. Preserve their wording, perspective, and uncertainty; do not turn it "
    "into a generic summary.\n"
    "2. Use Notion as a diary hierarchy, not a database. Find the page titled \"My Diary\" (a "
    "search, not a fetch by name — discover_tools/call_tool the same as any other Notion lookup), "
    "then fetch it and treat it as the diary's root. If it does not exist, or more than one "
    "plausible page exists, ask_user to decide whether/where to create it or which one to use.\n"
    "3. Check whether today's dated child page already exists using Notion search SCOPED to "
    "\"My Diary\" specifically — pass My Diary's own URL/ID as page_url (\"restrict keyword search "
    "to a page and its descendants\") and set filters.title_only=true, searching for today's "
    "YYYY-MM-DD date. A plain, unscoped workspace search is NOT reliable for this — it ranks and "
    "indexes independently of the page tree and can miss or bury a real, recently-created child "
    "page; the page_url-scoped search is the authoritative check for \"does today's page already "
    "exist,\" not a bare search. Match tolerantly: today's page may already exist from an earlier "
    "session with extra characters around the date (an emoji, a leading/trailing symbol) — if a "
    "result's title contains today's YYYY-MM-DD date, that IS today's page, use it rather than "
    "creating a second one. Only create a new dated child page when the scoped search genuinely "
    "finds nothing.\n"
    "4. Creating: title it with EXACTLY the plain YYYY-MM-DD string and nothing else — no emoji or "
    "other characters in the title text itself (an emoji belongs only in the page's own icon field, "
    "never baked into the title), so a future session's match stays reliable instead of drifting "
    "further — and set allow_async=false so the page is fully ready before you fetch it back to "
    "verify in the next step; the default (async) creation may not have real content yet if you "
    "fetch immediately after.\n"
    "5. Appending to an existing page: use the update command that inserts content rather than one "
    "that replaces the page's content wholesale — the entry must be added to whatever's already "
    "there, never overwrite it.\n"
    "6. Once written, you MUST fetch that same page back and confirm the entry text is genuinely "
    "present in the fetch result before telling the user it's saved — this fetch is mandatory, not "
    "optional. Only report success once that fetch's own result actually contains the entry text; "
    "if it doesn't, say so plainly and try again rather than reporting success anyway.\n\n"
    "STAYING IN OR LEAVING THIS MODE. Recording one entry does not end this activity — the user "
    "may have more to add in a follow-up message, and you should stay ready for that (this "
    "conversation keeps coming to you, not the ordinary assistant, until you explicitly leave). "
    "Call switch_mode() with no argument only when the user has clearly indicated they're done "
    "journaling for now (said so directly, moved on to an unrelated topic, or gone quiet after a "
    "natural close) — never automatically after just one entry, and never mid-entry before it's "
    "confirmed and verified.\n\n"
    "Every response, also call report_status alongside anything else you call: "
    "status=working while there is more to do, status=done when your message is the answer for "
    "this step, status=blocked when you cannot proceed without the user (pair it with ask_user). "
    "Set est_remaining_steps to your honest estimate of reasoning steps left, and note anything "
    "the next step needs to remember. Reaching status=done here is routine — it happens after "
    "every reply — and is completely separate from leaving journaling mode; only switch_mode "
    "does that."
)

register(SkillMode(name="journaling", description=DESCRIPTION, system_prompt=SYSTEM_PROMPT))
