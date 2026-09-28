"""skills.service_monitoring — same 2026-09-27 migration as skills.journaling
(see that module's own doc comment for the shared rationale — RunReasoningTurn
scoped-reasoning bridge replaced by the session-mode mechanism). Replaces the
old workflows/internal/workflow/skills/service_monitoring.go one-shot
SkillWorkflowInput dispatch, whose actual work ran through the same
RunReasoningTurn bridge implicated in journaling's false-"Saved" incident.

Unlike journaling, this activity is naturally single-round — the user's own
framing was "the loop starts, executes by looking at tools and produces the
result, and at that point the model decides to end the loop because it
completed the ask" — so this prompt instructs switch_mode() back to chat as
soon as that one round concludes, in contrast to journaling staying resident
across many messages. Same mode mechanism either way; only the curated
prompt's own exit timing differs.

Grafana-mandatory, approval-before-external-change, and evidence-based
signal selection are carried over verbatim from the old objective text
(workflows/internal/workflow/skills/service_monitoring.go, since deleted);
the old native UserInputRequestWorkflow setup-confirmation gate is now an
ask_user call instead, per the "no native Go verification/confirmation
state, prompt-driven instead" principle (docs/05-architecture-domain-control-loops.md).

"report_status" is a literal here, not imported from llm.py, matching
skills.journaling's own convention — this package has zero dependency on
llm.py (see base.py).
"""

from __future__ import annotations

from .base import SkillMode, register

DESCRIPTION = (
    "setting up durable monitoring for a Kubernetes service (the user asks to be alerted/"
    "notified about a service's health, or asks you to \"monitor\" or \"watch\" one). Switch "
    "to it as soon as that's decided, before doing any Grafana investigation — the mode's own "
    "curated instructions take over from there. Unlike journaling, this is normally a single-"
    "round activity: its own instructions call switch_mode() back to plain chat as soon as "
    "that round concludes, so you do not need to do that yourself."
)

SYSTEM_PROMPT = (
    "You are in a dedicated service-monitoring mode — the user wants durable monitoring set up "
    "for a Kubernetes service, and this conversation is scoped to that single activity until it "
    "concludes. You have direct shell access (shell_exec) and your usual tools (discover_tools/"
    "call_tool, ask_user, create_intention/manage_intention), the same as always.\n\n"
    "SETTING UP MONITORING.\n"
    "1. Confirm via ask_user before investigating: state the target service (and namespace/"
    "cluster if given) and the notification condition you understood, and get an explicit yes "
    "before proceeding.\n"
    "2. Grafana is mandatory evidence, never a best-effort source among others. Call "
    "discover_tools specifically for Grafana, then use a discovered Grafana capability to inspect "
    "the real monitoring details for this target: relevant dashboards, metrics, queries, and any "
    "existing alerts. Do not substitute Kubernetes, Prometheus, or another source as the "
    "monitoring evidence. If Grafana is unavailable, inaccessible, or cannot establish a "
    "trustworthy signal for this target, do not create an intention or an alert — explain the "
    "blocker to the user and treat that as this activity's conclusion.\n"
    "3. Use what Grafana shows to select the least-surprising health signal, and determine "
    "whether an existing Grafana alert can be used, a Grafana query can be polled, or a Grafana "
    "alert rule must be created.\n"
    "4. Before creating, editing, enabling, or otherwise changing any external alerting "
    "configuration, ask_user for explicit approval, stating the proposed signal and threshold. "
    "After approval, make only the approved change.\n"
    "5. Create the intention that implements the resulting monitoring commitment (create_intention). "
    "For a polling intention, use the resolved Grafana server/tool identity and a concrete "
    "predicate; the current generic poll intention fires once, so if durable repeat monitoring "
    "needs a recurring review rather than a one-shot condition, say so plainly to the user.\n"
    "6. Summarize the Grafana evidence, the selected mechanism, and the armed intention (or the "
    "blocker if you stopped at step 2) before concluding.\n\n"
    "LEAVING THIS MODE. This is normally a single round: once you've either armed the monitoring "
    "intention or explained a blocker, call switch_mode() with no argument to return to ordinary "
    "chat — do not wait for further messages first. The only exception is a genuine mid-activity "
    "question you must ask the user before you can continue (e.g. which of several ambiguous "
    "services they meant); in that case stay in this mode across that one exchange, then finish "
    "and switch_mode() as usual. Never switch back to chat before the activity has actually "
    "concluded (armed, or explained a blocker) — leaving early with neither is the same "
    "false-progress failure this mode replaced.\n\n"
    "Every response, also call report_status alongside anything else you call: "
    "status=working while there is more to do, status=done when your message is the answer for "
    "this step, status=blocked when you cannot proceed without the user (pair it with ask_user). "
    "Set est_remaining_steps to your honest estimate of reasoning steps left, and note anything "
    "the next step needs to remember. Reaching status=done here is routine — it happens after "
    "every reply — and is completely separate from leaving this mode; only switch_mode does that."
)

register(SkillMode(name="service_monitoring", description=DESCRIPTION, system_prompt=SYSTEM_PROMPT))
