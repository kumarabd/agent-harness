# Journaling skill

Journaling uses the [generic conversational skill contract](../05-architecture-domain-control-loops.md). The user explicitly selects `/skill journaling` or “let's journal”. Every later utterance still enters ordinary chat; unrelated questions and status checks do not become diary entries or deactivate journaling.

Chat submits/amends the exact proposed entry and presents its content-bound confirmation. Only explicit approval starts the bounded skill child. New diary content needs a new proposal and confirmation; completion never changes the user's selection.

## Domain work

The domain prompt directs the skill to locate one Notion page titled `My Diary`, fetch it, and search for a dated child scoped beneath that root. It must reuse the dated page when present and stop on an ambiguous/missing root. The date comes from the proposal; absent a date, the current implementation supplies UTC, not the user's local date. Chat should put the desired date in the approved proposal when that distinction matters.

The only external tools offered are `notion-search`, `notion-fetch`, `notion-create-pages`, and `notion-update-page`. Updates are restricted in code to `insert_content` with `position.type=end`; replacement and metadata edits are rejected. Mutations require `allow_async=false`. Creation is restricted to one dated page under a page parent. The content written must equal the exact confirmed text (apart from surrounding whitespace), and only one mutation is allowed per step. The policy targets the available Notion append schema, not legacy insertion commands; validate the tenant's discovered schema during rollout.

Success requires a fetch containing the approved text and a page reference associated with a successful write. A model's “done” or “saved” claim is not completion evidence. A lost write response, missing readback or budget exhaustion is a failed/unverified outcome, not permission to repeat a write automatically.

Page/root selection and date interpretation are model-directed under the domain prompt; append-only behavior, exact-content preservation, mutation count and readback evidence are code-enforced. The skill cannot ask/deliver directly, switch modes, call shell, or spawn a subagent. Ordinary chat reports the result or asks for missing information.

## Implementation and limits

Domain logic: `activities/activities/skills/journaling.py`.
Shared execution: `skill_runtime.py`, `workflows/internal/workflow/skill.go`, and `coordinator.go`.

The current step does not create a missing diary root, perform multiple entry writes, undo writes, or bypass tenant permission rules. Cancellation can stop pending work but cannot guarantee that an already-started provider write was undone. See the shared design for retry and rollout requirements.
