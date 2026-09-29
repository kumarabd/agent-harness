import json
import unittest
from dataclasses import asdict
from datetime import datetime, timedelta, timezone
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock, patch

from activities.skills import journaling, service_monitoring
from activities.types import Message, UserInputRequest
from activities.user_input import RequestUserInputActivity
from temporalio.converter import DataConverter
from temporalio.exceptions import ApplicationError

from activities import skill_runtime as runtime


class SelectionTests(unittest.TestCase):
    def test_only_explicit_full_message_commands_select(self):
        for text, expected in [
            ("Let's journal!", "journaling"),
            ("/skill service_monitoring", "service_monitoring"),
            ("stop journaling", "chat"),
            ("/chat", "chat"),
        ]:
            self.assertEqual(runtime.selection_command(text), expected)
        for text in [
            "How is my journal?",
            "I went to work today",
            "yes",
            "please start journaling after answering this",
            "what is the weather?",
        ]:
            self.assertIsNone(runtime.selection_command(text))

    def test_cancel_requires_a_direct_command(self):
        for text in ("cancel", "Please stop this step.", "abort the running skill task"):
            self.assertTrue(runtime.explicit_cancel(text), text)
        for text in ("Don't stop, keep journaling", "I can't stop thinking about this", "never cancel this"):
            self.assertFalse(runtime.explicit_cancel(text), text)


class CommandTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.pool = SimpleNamespace(
            fetchrow=AsyncMock(),
            fetchval=AsyncMock(return_value=True),
            execute=AsyncMock(),
        )
        self.activities = runtime.SkillActivities(self.pool, Mock())
        self.state = runtime.ConversationState(
            "journaling",
            runtime.SkillState(
                kind="journaling",
                content_id="proposal",
                revision=2,
                phase="awaiting_confirmation",
            ),
        )
        self.input = runtime.SkillCommandInput("call", "session", self.state)
        self.args = {"action": "confirm", "revision": 2}
        self.now = datetime.now(timezone.utc)
        self.source = {
            "message_id": "m",
            "speaker_id": "user",
            "content": "yes",
            "created_at": self.now,
        }
        self.request = {
            "status": "cancelled",
            "selected_option_id": None,
            "free_text": None,
            "created_at": self.now - timedelta(seconds=1),
        }

    def rows(self, *, outcome=None, tool="skill_command"):
        self.pool.fetchrow.side_effect = [
            {
                "arguments": json.dumps(self.args),
                "result": outcome,
                "tool_name": tool,
                "parent_id": "session:turn:1",
            },
            self.source,
            self.request,
        ]

    async def test_cold_start_preserves_selection(self):
        self.pool.fetchval.return_value = "journaling"
        self.assertEqual(await self.activities.load_mode("session"), "journaling")
        self.pool.execute.assert_not_awaited()

    async def test_headless_session_bootstrap_does_not_overwrite_selection(self):
        self.pool.fetchval.side_effect = [None, "chat"]
        self.assertEqual(await self.activities.load_mode("session"), "chat")
        self.assertIn(
            "ON CONFLICT (session_key) DO NOTHING", self.pool.execute.call_args.args[0]
        )

    async def test_spoken_approval_is_content_bound(self):
        self.rows()
        result = await self.activities.prepare(self.input)
        self.assertEqual(result.action, "confirm")
        self.assertEqual(result.content_id, "proposal")
        self.assertEqual(self.pool.fetchrow.call_args.args[1:], ("session", "proposal"))

    async def test_button_approval_is_accepted(self):
        self.source["content"] = "my entry"
        self.request.update(status="answered", selected_option_id="approve")
        self.rows()
        self.assertEqual((await self.activities.prepare(self.input)).action, "confirm")

    async def test_status_question_is_not_consent(self):
        self.source["content"] = "did you save it?"
        self.rows()
        with self.assertRaisesRegex(ApplicationError, "explicit approval"):
            await self.activities.prepare(self.input)

    async def test_denial_cannot_be_overridden_by_old_yes(self):
        self.request.update(status="answered", selected_option_id="deny")
        self.rows()
        with self.assertRaisesRegex(ApplicationError, "explicit approval"):
            await self.activities.prepare(self.input)

    async def test_approval_before_prompt_is_rejected(self):
        self.source["created_at"] = self.now - timedelta(seconds=2)
        self.rows()
        with self.assertRaisesRegex(ApplicationError, "explicit approval"):
            await self.activities.prepare(self.input)

    async def test_missing_prompt_is_rejected(self):
        self.request = None
        self.rows()
        with self.assertRaisesRegex(ApplicationError, "present the exact proposal"):
            await self.activities.prepare(self.input)

    async def test_stale_revision_is_rejected_before_content_write(self):
        self.args.update(action="amend", content="new", revision=1)
        self.rows()
        with self.assertRaisesRegex(ApplicationError, "stale"):
            await self.activities.prepare(self.input)
        self.pool.execute.assert_not_awaited()

    async def test_synthetic_input_cannot_authorize(self):
        self.source["speaker_id"] = None
        self.rows()
        with self.assertRaisesRegex(ApplicationError, "real user"):
            await self.activities.prepare(self.input)

    async def test_ownership_is_required(self):
        self.pool.fetchrow.return_value = None
        with self.assertRaisesRegex(ApplicationError, "belong to this session"):
            await self.activities.prepare(self.input)

    async def test_command_audit_deduplicates_repeated_update(self):
        self.rows(outcome=json.dumps({"skill_command": True}))
        self.assertTrue((await self.activities.prepare(self.input)).already_applied)
        self.pool.execute.assert_not_awaited()

    async def test_confirmed_step_outcome_replaces_running_snapshot(self):
        self.pool.execute.return_value = "UPDATE 1"
        self.state.skill.step_id = "call"
        for phase, status, side_effect in [
            ("completed", "ok", None),
            ("failed", "error", None),
            ("cancelled", "cancelled", "unknown"),
        ]:
            self.state.skill.phase = phase
            await self.activities.record_outcome("call", self.state, "step stopped")
            args = self.pool.execute.call_args.args
            self.assertEqual(args[1], "call")
            self.assertEqual(args[2], status)
            self.assertEqual(args[4], "step stopped")
            self.assertEqual(args[5], side_effect)
            result = json.loads(args[3])
            self.assertTrue(result["skill_step_terminal"])
            self.assertEqual(result["state"]["skill"]["phase"], phase)

    async def test_amendment_is_immutable_new_content(self):
        self.args.update(action="amend", content="exact new text")
        self.rows()
        result = await self.activities.prepare(self.input)
        self.assertEqual(result.content_id, "call")
        self.assertEqual(
            self.pool.execute.call_args.args[1:],
            ("call", "session", "journaling", "exact new text"),
        )

    async def test_running_step_rejects_amendment(self):
        self.state.skill.phase = "running"
        self.args.update(action="amend", content="new")
        self.rows()
        with self.assertRaisesRegex(ApplicationError, "cancel and wait"):
            await self.activities.prepare(self.input)

    async def test_disabled_skill_cannot_start(self):
        self.pool.fetchval.return_value = False
        self.rows()
        with self.assertRaisesRegex(ApplicationError, "no enabled"):
            await self.activities.prepare(self.input)

    async def test_cancel_requires_explicit_user_request(self):
        self.args["action"] = "cancel"
        self.rows()
        with self.assertRaisesRegex(ApplicationError, "explicit user request"):
            await self.activities.prepare(self.input)
        self.source["content"] = "cancel that"
        self.rows()
        self.assertEqual((await self.activities.prepare(self.input)).action, "cancel")

    async def test_selection_requires_real_user_and_live_enablement(self):
        result = await self.activities.select(
            runtime.UserSelectionInput("session", Message(role="user", content="/chat"))
        )
        self.assertEqual(result, "")
        self.pool.execute.assert_not_awaited()
        result = await self.activities.select(
            runtime.UserSelectionInput(
                "session",
                Message(
                    role="user", content="/chat", speaker_id="user", client_msg_id="m"
                ),
            )
        )
        self.assertEqual(result, "chat")
        self.assertEqual(self.pool.execute.call_args.args[1:], ("session", "chat", "m"))

    async def test_temporal_payload_round_trip(self):
        converter = DataConverter.default
        payloads = await converter.encode([asdict(self.input)])
        [decoded] = await converter.decode(payloads, [runtime.SkillCommandInput])
        self.assertEqual(decoded, self.input)

    async def test_confirmation_prompt_cannot_be_model_substituted(self):
        self.pool.fetchrow.side_effect = [
            {
                "arguments": json.dumps(
                    {
                        "question": "Approve something else",
                        "skill_content_id": "proposal",
                        "options": ["yes"],
                    }
                )
            },
            {"content": "exact entry"},
        ]
        await RequestUserInputActivity(self.pool)(
            UserInputRequest(request_id="r", turn_id="session:turn:1", kind="question"),
            "w",
        )
        args = self.pool.execute.call_args.args
        self.assertEqual(args[5], "Approve this exact proposal?\n\nexact entry")
        self.assertEqual(json.loads(args[6])[0]["id"], "approve")
        self.assertEqual(json.loads(args[8]), {"skill_content_id": "proposal"})

    async def test_snapshot_only_reads_current_step(self):
        self.state.skill.step_id = "current-step"
        self.pool.fetchval.return_value = "entry"
        client = Mock()
        client.get_workflow_handle.return_value.query = AsyncMock(
            return_value=asdict(self.state)
        )
        self.pool.fetchrow.return_value = None
        control, reference = await runtime.conversation_context(self.pool, client, "session:turn:1")
        self.assertEqual(self.pool.fetchrow.call_args.args[1:], ("current-step",))
        self.assertNotIn("entry", control)
        self.assertIn("entry", reference)


PAGE = "12345678-1234-1234-1234-123456789abc"
OTHER_PAGE = "aaaaaaaa-1234-1234-1234-123456789abc"


class DomainPolicyTests(unittest.TestCase):
    def test_journal_is_append_only_and_preserves_approved_text(self):
        with self.assertRaisesRegex(ValueError, "never replace"):
            journaling.validate(
                "notion-update-page",
                {"command": "replace_content", "content": "entry"},
                "entry",
                [],
            )
        with self.assertRaisesRegex(ValueError, "exact confirmed"):
            journaling.validate(
                "notion-update-page",
                {
                    "command": "insert_content",
                    "position": {"type": "end"},
                    "content": "different",
                    "allow_async": False,
                },
                "entry",
                [],
            )
        journaling.validate(
            "notion-update-page",
            {
                "command": "insert_content",
                "position": {"type": "end"},
                "content": "entry",
                "allow_async": False,
            },
            "entry",
            [],
        )

    def test_journal_rejects_second_or_async_mutation(self):
        creation = {
            "parent": {"page_id": PAGE},
            "pages": [{"properties": {"title": "2026-09-28"}, "content": "entry"}],
        }
        with self.assertRaisesRegex(ValueError, "allow_async"):
            journaling.validate("notion-create-pages", creation, "entry", [])
        creation["allow_async"] = False
        journaling.validate("notion-create-pages", creation, "entry", [])
        creation["pages"].append(creation["pages"][0])
        with self.assertRaisesRegex(ValueError, "exactly one"):
            journaling.validate("notion-create-pages", creation, "entry", [])
        with self.assertRaisesRegex(ValueError, "one diary mutation"):
            journaling.validate(
                "notion-create-pages",
                {"content": "entry", "allow_async": False},
                "entry",
                [{"request": {"tool": "notion-create-pages"}}],
            )

    def test_journal_completion_requires_same_page_readback(self):
        mutation = {
            "request": {"tool": "notion-update-page", "arguments": {"page_id": PAGE}},
            "response": {"ok": True},
        }
        current = {
            "request": {"tool": "notion-fetch", "arguments": {"id": PAGE}},
            "response": {
                "content": [
                    {"text": json.dumps({"entry": "a line\nwith two paragraphs"})}
                ]
            },
        }
        self.assertTrue(
            journaling.complete("a line\nwith two paragraphs", current, [mutation])
        )
        self.assertFalse(
            journaling.complete("a line\nwith two paragraphs", current, [])
        )
        current["request"]["arguments"]["id"] = OTHER_PAGE
        mutation["request"]["arguments"]["content"] = OTHER_PAGE
        self.assertFalse(
            journaling.complete("a line\nwith two paragraphs", current, [mutation])
        )

    def test_journal_rejects_unapproved_metadata_and_extra_content(self):
        args = {
            "command": "insert_content",
            "position": {"type": "end"},
            "content": "entry",
            "allow_async": False,
            "properties": {"title": "different"},
        }
        with self.assertRaisesRegex(ValueError, "metadata"):
            journaling.validate("notion-update-page", args, "entry", [])
        del args["properties"]
        args["content"] = "entry plus unapproved text"
        with self.assertRaisesRegex(ValueError, "exact confirmed"):
            journaling.validate("notion-update-page", args, "entry", [])

    def test_monitor_requires_observed_grafana_probe(self):
        args = {"kind": "condition", "probe": {"tool": "grafana/query_prometheus"}}
        with self.assertRaisesRegex(ValueError, "evidence"):
            service_monitoring.validate("create_intention", args, "watch service", [])
        history = [
            {
                "request": {"tool": "query_prometheus", "server": "grafana"},
                "response": {"value": 1},
            }
        ]
        service_monitoring.validate("create_intention", args, "watch service", history)
        args["probe"]["tool"] = "other/delete_data"
        with self.assertRaisesRegex(ValueError, "actually inspected"):
            service_monitoring.validate(
                "create_intention", args, "watch service", history
            )

    def test_existing_intention_dedup_is_reported_as_complete(self):
        current = {
            "request": {"tool": "create_intention"},
            "response": {
                "intention_id": "id",
                "note": "an intention with a matching objective is already armed — revise or cancel it instead",
            },
        }
        self.assertTrue(service_monitoring.complete("", current, []))

    def test_freshly_armed_intention_is_reported_as_complete(self):
        current = {
            "request": {"tool": "create_intention"},
            "response": {"intention_id": "id", "armed": True},
        }
        self.assertTrue(service_monitoring.complete("", current, []))

    def test_missing_intention_id_is_not_reported_as_complete(self):
        current = {
            "request": {"tool": "create_intention"},
            "response": {"note": "something went sideways"},
        }
        self.assertFalse(service_monitoring.complete("", current, []))

    def test_errored_create_intention_is_not_reported_as_complete(self):
        current = {
            "request": {"tool": "create_intention"},
            "response": {"intention_id": "id", "error": "boom"},
        }
        self.assertFalse(service_monitoring.complete("", current, []))


class ExecutionTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.pool = SimpleNamespace(
            fetch=AsyncMock(),
            fetchval=AsyncMock(),
            fetchrow=AsyncMock(
                return_value={"content": "entry", "session_key": "session"}
            ),
            execute=AsyncMock(),
        )
        self.activities = runtime.SkillActivities(self.pool, Mock())
        self.iteration = runtime.SkillIteration("step", "proposal", "journaling", 0)

    async def test_reason_returns_committed_decision_after_competing_retry(self):
        self.pool.fetch.return_value = []
        self.pool.fetchval.side_effect = [
            "entry",
            json.dumps({"tool": "notion-update-page", "mutation": True}),
        ]
        provider = SimpleNamespace(
            call_model=AsyncMock(
                return_value=SimpleNamespace(
                    content="look up diary",
                    raw_tool_calls=[
                        {"name": "discover_tools", "arguments": {"query": "diary"}}
                    ],
                )
            )
        )
        config = SimpleNamespace(context_window=32000, model="test", max_tokens=1000)
        with (
            patch.object(
                runtime.model_registry,
                "default_hint",
                return_value=("language", "medium"),
            ),
            patch.object(runtime.model_registry, "resolve", return_value=config),
            patch.object(runtime.llm_client, "get_provider", return_value=provider),
        ):
            decision = await self.activities.reason(self.iteration)
        self.assertTrue(decision.mutation)
        self.assertTrue(decision.has_call)
        offered = provider.call_model.call_args.args[3]
        self.assertEqual(
            [tool["function"]["name"] for tool in offered], ["discover_tools"]
        )
        written = json.loads(self.pool.execute.call_args.args[3])
        self.assertEqual(written["tool"], "discover_tools")

    async def test_reason_rejects_model_attempt_to_use_unoffered_tool(self):
        self.pool.fetch.return_value = []
        self.pool.fetchval.return_value = "entry"
        provider = SimpleNamespace(
            call_model=AsyncMock(
                return_value=SimpleNamespace(
                    content="",
                    raw_tool_calls=[{"name": "shell_exec", "arguments": {}}],
                )
            )
        )
        config = SimpleNamespace(context_window=32000, model="test", max_tokens=1000)
        with (
            patch.object(
                runtime.model_registry,
                "default_hint",
                return_value=("language", "medium"),
            ),
            patch.object(runtime.model_registry, "resolve", return_value=config),
            patch.object(runtime.llm_client, "get_provider", return_value=provider),
            self.assertRaisesRegex(ApplicationError, "outside this skill"),
        ):
            await self.activities.reason(self.iteration)
        self.pool.execute.assert_not_awaited()

    async def test_checkpointed_decision_is_reused_without_model(self):
        self.pool.fetch.return_value = [
            {
                "request": {"tool": "notion-update-page", "mutation": True},
                "response": None,
            }
        ]
        with patch.object(runtime.llm_client, "get_provider") as provider:
            decision = await self.activities.reason(self.iteration)
        self.assertTrue(decision.mutation)
        provider.assert_not_called()

    async def test_checkpointed_response_is_reused_without_external_call(self):
        self.pool.fetch.return_value = [
            {"request": {"tool": "discover_tools"}, "response": {"results": []}}
        ]
        with patch.object(runtime.mcp_hub, "call_tool", new_callable=AsyncMock) as call:
            result = await self.activities.execute(self.iteration)
        self.assertFalse(result.complete)
        call.assert_not_awaited()

    async def test_discovery_filters_wrong_backend_and_unapproved_tools(self):
        self.pool.fetch.return_value = [
            {
                "request": {
                    "tool": "discover_tools",
                    "server": "",
                    "arguments": {"query": "search"},
                },
                "response": None,
            }
        ]
        rows = [
            {"server": "notion", "tool": "notion-search", "input_schema": {}},
            {"server": "other", "tool": "notion-search", "input_schema": {}},
            {"server": "notion", "tool": "delete-everything", "input_schema": {}},
        ]
        with (
            patch.object(
                runtime.mcp_hub,
                "call_tool",
                new_callable=AsyncMock,
                return_value={"result": rows},
            ),
            patch.object(runtime.activity, "heartbeat"),
        ):
            await self.activities.execute(self.iteration)
        response = json.loads(self.pool.execute.call_args.args[3])
        self.assertEqual(response["results"], rows[:1])

    async def test_permission_policy_is_rechecked_at_execution(self):
        self.iteration.index = 1
        self.pool.fetch.return_value = [
            {
                "request": {"tool": "discover_tools"},
                "response": {
                    "results": [
                        {"server": "notion", "tool": "notion-fetch", "input_schema": {}}
                    ]
                },
            },
            {
                "request": {
                    "tool": "notion-fetch",
                    "server": "notion",
                    "arguments": {"id": PAGE},
                },
                "response": None,
            },
        ]
        with (
            patch.object(runtime.permissions, "requires_approval", return_value=True),
            patch.object(runtime.mcp_hub, "call_tool", new_callable=AsyncMock) as call,
            self.assertRaisesRegex(ApplicationError, "separate permission"),
        ):
            await self.activities.execute(self.iteration)
        call.assert_not_awaited()

    async def test_unoffered_tool_cannot_execute(self):
        self.pool.fetch.return_value = [
            {
                "request": {"tool": "shell_exec", "server": "", "arguments": {}},
                "response": None,
            }
        ]
        with self.assertRaisesRegex(ApplicationError, "identity is not authorized"):
            await self.activities.execute(self.iteration)
