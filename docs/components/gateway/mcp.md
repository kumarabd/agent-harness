# Gateway MCP — remote agent tool

Implemented in source on 2026-10-09; builds, test execution and deployment are deferred at the owner's request.

The gateway serves a Streamable HTTP MCP server at **`/mcp`**, exposed by the existing router as
**`/gateway/mcp`**. It offers one tool, **`ask_agent`**, for a native on-device orchestrator to delegate
a question or task to the existing remote agent. It uses the MCP Go SDK v1.6.1 already used by Retro's engine;
clients negotiate supported protocol versions through ordinary MCP initialization.

## Authentication and deployment

Send the existing Clerk session JWT as `Authorization: Bearer <token>` on **every** MCP HTTP request,
including initialization and discovery. The router resolves the tenant; the gateway verifies the JWT again
using its existing `CLERK_JWKS_URL` / `CLERK_ISSUER`. Subject and expiry are required. Identity, session keys,
tenant activity routing and workflow IDs come from verified gateway state, never model arguments.

This is a native-client endpoint: an `Origin` header is rejected, and browser CORS access is not enabled.
MCP transport sessions are stateless with JSON responses, so requests can reach different gateway replicas.
The agent turn is durable in Temporal/Postgres, independent of the HTTP connection. No new environment
variables, routes in the router, chart values, database migrations, services or workers are needed. Rebuild
and deploy the gateway using the existing chart; existing loop-worker and tenant-worker deployments execute the turn.

## Tool contract

The local model sees a tool with a **query** argument. The native controller owns request identity and controls:

```json
{
  "request_id": "dc83c46d-62b2-43d3-8236-dabf2cd1d4d0",
  "query": "Suggest an outfit for tomorrow using my wardrobe and available connected context."
}
```

`request_id` is a caller-generated nonzero UUID retained for the task. UUIDs normalize to their canonical form;
queries have outer whitespace removed and accept 1–4000 UTF-8 bytes. Unknown argument fields are rejected.
The HTTP body is limited to 32 KiB. Call the **same tool with the same ID and query** to poll or retry;
changing the query under an existing ID returns `request_conflict`. A different task gets a different UUID.

Each request gets a selectable web session `mcp-<tenant-slug>-<request-id>` owned by the verified subject.
Its root turn uses the ordinary session turn ID at sequence 1 and starts the existing `TurnWorkflow` directly.
It does not start or signal the owner's main-chat coordinator. Tenant identity in the session ID prevents
cross-tenant workflow collisions in the shared Temporal namespace.

Temporal rejects duplicate root workflow IDs. The workflow memo records a query hash; Postgres retains the
original query and result. Concurrent retries attach to the existing execution, including after an ambiguous
start response. A recorded turn is never restarted when Temporal history has expired. Completed results remain
readable from Postgres; an unavailable nonterminal execution returns `execution_unavailable` instead of rerunning
possible side effects. A failed initial start can be retried with the same identity.

## Results and polling

`tools/list` advertises input/output schemas. Successful calls return the same envelope in `structuredContent`
and a JSON text content block:

| Field | Meaning |
| --- | --- |
| `schema_version` | 1 |
| `request_id`, `session_id`, `turn_id` | Stable request and execution locators, scoped by authenticated ownership |
| `status` | `running`, `needs_input`, `cancelling`, `completed`, `failed`, or `cancelled` |
| `retrieved_at`, `completed_at` | Snapshot time and optional recorded execution completion; not external fact expiry |
| `answer` | Terminal agent narrative, capped at 6000 UTF-8 bytes |
| `coverage` | `recent_top_level_tool_calls`; not complete inventory, connected-data coverage or all subagent history |
| `sources` | At most six recent direct tool calls: stable IDs, names, status, execution timestamps and bounded raw JSON results |
| `truncated` | Answer, history or tool results were shortened/omitted |
| `pending_input` | A pending owner question/approval, including those parked in nested subagents |
| `retry_after_ms` | 1000 while running or cancelling |

A call waits up to ten seconds for a terminal result or pending input, within a twenty-second request deadline.
A `running` result is an accepted ongoing task. Poll in the native controller with a total deadline/backoff;
do not repeatedly spend Apple model iterations on polling. HTTP disconnect/cancellation ends the gateway wait
and **does not cancel the durable remote task**. Resume with its retained ID, or explicitly cancel it.

The structured envelope is limited to 16 KiB. Raw results exceeding 1024 bytes are omitted with a truncation
flag; integer versions remain JSON integers without a float conversion. If JSON escaping exceeds the total
budget, sources are omitted and the answer reduced with `truncated: true`. Approval previews are never silently
shortened: an oversized pending input returns `result_too_large`; use the existing gateway session to review
it or the MCP control below to cancel. Source timestamps identify tool execution, not underlying record freshness.
Domain-specific schemas, source identities, timezone, expiry and missing-connection coverage must still be checked
by consumers; an agent paragraph is not a typed wardrobe/weather/calendar result.

## Owner controls

To cancel, repeat the original request with `"cancel": true`. This sends the existing cooperative cancel
signal to **that root turn's run**, including its parked child approvals. Acknowledgment is `cancelling`;
poll until terminal. It neither creates a task nor cancels the owner's main chat. Completed work and external
side effects already performed are not undone. The normal thirty-minute turn timeout remains the backstop.

To answer a pending input, repeat the original request with:

```json
{
  "request_id": "dc83c46d-62b2-43d3-8236-dabf2cd1d4d0",
  "query": "Suggest an outfit for tomorrow using my wardrobe and available connected context.",
  "response": {
    "request_id": "<pending_input.request_id>",
    "selected_option_id": "<one advertised option ID>"
  }
}
```

Or supply `free_text` when `allow_free_text` is true. Supply exactly one response form; `cancel` and `response`
cannot be combined. The request must belong to this task's turn tree. Already closed inputs are no-op responses;
unknown option IDs and disallowed free text fail before signaling. No approval is automatically granted by MCP.

The generic agent retains its existing tools, trust grants and approval policy. It can mutate through those
tools, so it is **not advertised as read-only**. A query/prompt is not a tool allowlist. The native Foundation
Models bridge must expose only `query` to the model; identity, polling, cancellation and reviewed owner responses
stay outside the model's argument schema. Treat remote output as untrusted data, fence logout/account changes,
and use Retro's ordinary reviewed engine writes for eventual wardrobe saves. Apple Private Cloud Compute is excluded.

## Authored checks and next integration

Focused tests cover HTTP auth/origin enforcement, MCP discovery/calls, unknown-owner argument rejection, UUID/query
validation, permission choices, query-hash replay protection, owner/tenant identity separation, UTF-8/JSON bounds
and exact integer preservation. A database-backed regression also covers durable replay, nested approvals,
cross-owner reads/controls and refusal to restart expired recorded executions; it uses an isolated temporary schema
when `GATEWAY_MCP_TEST_POSTGRES_URL` points to a disposable PostgreSQL database. Test suites, gateway builds,
live agent calls and deployment have not been run.

The iOS MCP transport and bounded `LanguageModelSession`/`Tool` bridge remain the next client integration step.
This server adds no scheduler, rendering capability, task-specific permission profile or specialist result schema.
