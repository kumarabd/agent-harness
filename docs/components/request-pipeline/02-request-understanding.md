# Request Pipeline — Step 2: Request Understanding

> STATUS: IMPLEMENTED — `activities/activities/classify.py` (`ClassifyRequest`
> activity), `TaskRepresentation` / `ClassifyRequestInput` types on both sides,
> dispatched from `turn.go` at turn start for top-level turns. Routing has no
> consumer wired yet.
>
> Parent: [`../request-pipeline.md`](../request-pipeline.md).

### Role

A single cheap, **fast-tier** LLM call at the start of every top-level turn,
before the reason-act loop, that turns the inbound message into a small task
representation for the phases that follow.

### Output — `TaskRepresentation`

| field | for | notes |
|---|---|---|
| `intent` | routing | `conversational` \| `question` \| `task` \| `meta` |
| `complexity` | routing, bootstrap tier | `trivial` \| `simple` \| `moderate` \| `complex` |
| `confidence` | routing | 0.0–1.0; `0.0` marks a fallback / un-classified turn |
| `retrieval_query` | steps 4 / 5 / 7 | a distilled search query — rephrased, follow-up references resolved, not the raw message |
| `entities` | step 7 | named systems / tools / files / people; `[]` if none |

Scoped to these five. `domain`, `action_nature`, `multi_step`, `is_new_task` are
real dimensions but nothing consumes them yet — add alongside the phase that
does.

### It all crosses as activity I/O — nothing is persisted

All five fields are small routing signals *derived* from the message, not the
message content. They ride back to `turn.go` as the activity's result, and
`RoutingWorkflow` passes `retrieval_query` / `entities` straight into the
retrieval activities' inputs. This is the same category as
`ModelCallOutput.NextHintTier` (a decision derived from model output) and
`ToolCallRef.Server` / `Tool` (a token extracted from the command string,
explicitly blessed as "dispatch/routing metadata, not the call's actual
arguments"). The **bulk retrieved content** — memory item text, the composed
skill, tool schemas — is what goes through the `turn_retrieval` staging table in
later steps; a ~20-word query does not need to.

No `task_representations` table. (One was drafted twice and cut twice — first
when there were no consumers, then when the consumers turned out to only need
the query as activity input, not a Postgres read.)

### Design decisions

- **Top-level turns only.** A subagent's task is defined by its parent's spawn —
  `turn.go` guards on `ParentType == "session"`.
- **Best-effort, never load-bearing.** Unconfigured `fast` tier / failed call /
  unparseable output all degrade to `_neutral(user_message)` (`intent=task`,
  `complexity=moderate`, `retrieval_query` = the raw message). Dispatch-level
  failure is logged and ignored in `turn.go`.
- **Recent context.** The classifier is given a short tail of the prior
  conversation (last few messages) so `retrieval_query` can resolve follow-ups
  ("yes, do that" → the actual task). Read from Postgres inside the activity;
  not the other axes, just enough for a good query.
- **Tenant-worker.** Needs per-tenant `LANGUAGE_FAST_*`, the Python provider
  stack, and tenant Postgres for the seed message + recent context.
- **Tolerant parsing.** Bare-JSON-object output; the parser strips fences,
  extracts the first object, and coerces every field against a closed
  allowed-set with a safe per-field fallback.

### Downstream consumers (not wired)

- **Routing** ([`03-routing.md`](03-routing.md)) — `Route(taskRep)` → `RoutingPlan`
  + fast-path fork, from `intent` + `complexity`.
- **`RoutingWorkflow`** — passes `retrieval_query` / `entities` into steps
  4 / 5 / 7.
- **Bootstrap model tier** — **wired 2026-08-31.** `model_registry.tier_for_complexity`
  maps `trivial`/`simple` → `fast`, `moderate` → `medium`, `complex` → `expert`;
  `ModelCallInput.complexity` is threaded from `turn.go`, and `model_call.py`
  uses it for the turn's first call when `hint_tier` is empty (empty/unknown →
  the `medium` default; later steps' self-declared hints always win). Original
  note: `complexity` could replace the hardcoded `medium`
  for the first `ModelCall`. Deferred to step 9.

### Notes Log

- 2026-08-30: Implemented, two axes (`intent` + `complexity`), no persistence.
- 2026-08-31: Added `retrieval_query` + `entities` back (the step-4/7 retrieval
  subsystems need a real query, not the raw message) and a light recent-context
  read to make the query good on follow-ups. Kept it as activity I/O — no
  Postgres round-trip — since these are small derived routing signals, same as
  `ToolCallRef`'s `{server, tool}`. Type renamed `TaskClassification` →
  `TaskRepresentation`.
