# Graph Report - .  (2026-09-24)

## Corpus Check
- 367 files · ~287,452 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 1681 nodes · 2715 edges · 140 communities (128 shown, 12 thin omitted)
- Extraction: 92% EXTRACTED · 8% INFERRED · 0% AMBIGUOUS · INFERRED: 221 edges (avg confidence: 0.74)
- Token cost: 658,526 input · 0 output

## Community Hubs (Navigation)
- Realtime Session Access & Tailing
- Core Activities: DB & Context
- VAD/EOT Sidecar Models
- Discord Voice Delivery & Components
- Architecture Topology & Delivery Decisions
- IDs, Claim-Check & Subagent Manifest
- LCM Context Assembly & Compaction
- VAD Sidecar Protobuf Messages
- Gateway Ingestion & Main
- EOT Sidecar gRPC Protobuf
- Discord Delivery & Reply Mode
- Coordinator & Scenario Scaffolding
- Clerk Auth & Realtime Handler
- Capability Table & Tool Schema
- Intention Tool Activities
- ModelCall Activity
- Shared Workflow Types (Go)
- Skill Workflows: Draft Note & Journaling
- LLM Provider Abstraction
- Exploration Summary & Claim-Check Helpers
- LLM Integration & Report Status
- Proactivity & Project Authoring Docs
- Turn Workflow Core Loop
- Agent-Brain Retain Client
- Tenant Helm Chart & Jobs
- Session Directory Leases
- Discord Bot Core
- Session Coordinator & State Layer Docs
- Discord Gateway Deployment & Voice Docs
- Shared Pool & Multi-Tenancy Docs
- Voice EOT & Silero VAD Client
- Shell-Hub Tool Discovery
- ToolCall Activity
- Reason-Act Loop Types & Dispatch
- Discord Voice Connection Lifecycle
- WhisperLive Streaming STT Client
- Model Registry & Provider Cache
- Agent-Web & Agent-Brain Deploy
- Voice Filler-Phrase Player
- Discord Voice State & Commands
- LLM Tier Config & No-Fallback
- First-Party Realtime Gateway Docs
- Mobile/Web Presence & Session Docs
- Web Session & Context-Slot Docs
- Discord User-Input Interactions
- Recursion Guard & Future Work
- Realtime Wire Protocol Frames
- Intention Workflow Tests
- Skill Call Activities
- Skill Registry & Discovery
- Voice DSP: Barge-In, Filler, VAD
- Voice Latency Tracking
- Regression Suite & Registry Docs
- Web Gateway Auth
- Web Cancel & Sessions Handlers
- MCP-Hub Client
- Session Filesystem & PVC Docs
- Voice EOT Client
- Voice Barge-In Mechanism
- User-Input Request Workflow
- Loop-Worker Main
- InsertMessage Activity
- Skill Reasoning Outcome
- Energy VAD & Speaker Buffer
- Interrupt Model & Button Routing Docs
- Gateway Lease Manager
- Mobile Briefings
- Mobile Briefing Handlers
- Intention Workflow Core
- Permission Gating
- StatusPing Activity
- README Architecture Summary & Shared Deploy
- Discord Voice Client Setup
- Discord Bot Setup
- Session Key Derivation
- Web Session ID Mapping
- Voice Text Normalization & Prompt Bug
- Voice Lifecycle State Machine
- Deep-Conversation Scenario Runner
- Superpowers B1 Scenario Test
- Superpowers B2 Scenario Test
- Context-Slot & Budget-Guardrails Concepts
- Web Poll Response Types
- Anthropic-Basic Scenario Test
- Ask-User-Followup Scenario Test
- Blocked-Terminal Scenario Test
- Ceiling-Raise Scenario Test
- Claim-Check-Large-Output Scenario Test
- Discover-Skill-Dispatch Scenario Test
- Done-With-Empty-Content Scenario Test
- Done-With-Tool-Calls Scenario Test
- Exploration-Summary-CSV Scenario Test
- Exploration-Summary-JSON Scenario Test
- Exploration-Summary-Text Scenario Test
- Happy-Path Scenario Test
- Interrupt-Followup Scenario Test
- LCM-Grep-Nested-Fold Scenario Test
- LCM-Retrieval Scenario Test
- Lite-Simple-Task Scenario Test
- Max-Iterations Scenario Test
- No-Progress-Failure Scenario Test
- No-Progress-Guard Scenario Test
- Parallel-Subagents Scenario Test
- Real-Assembly Scenario Test
- Resolved-Tool-Dispatch Scenario Test
- Shell-Exec-Basic Scenario Test
- Shell-Exec-Slow Scenario Test
- Skill-Interrupt-Followup Scenario Test
- Spawn-Subagent-Nested-Rejected Test
- Spawn-Subagent-Nested-Valid Test
- Subagent-Full-Agent Scenario Test
- Subagent-Spawn Scenario Test
- Cancel Primitive Test Script
- Sentence Boundary Segmenter
- Discord Ambient Message Buffer
- Cancel Primitive & Shared Access Docs
- Progress Narration & Wire Protocol Docs
- Mobile Live Smoke Test
- Web Poll Handler
- Web Respond Handler
- Web Send Handler
- Tool Timing Tiers
- Scenario Runner Script
- Grafana Snapshot & Metrics Config
- Gateway-Per-Platform Topology Doc
- Draft-Note Skill & UseSkill Field Doc
- Web Cancel Request Types
- Web Sessions List Types
- Web Respond Request Types
- Web Send Request Types
- Regression Suite Runner
- Test Data Cleanup Script
- Outbound Delivery Doc Stub
- Activities Package Metadata
- Workflows Go Module
- VAD-Sidecar Package Metadata

## God Nodes (most connected - your core abstractions)
1. `main()` - 25 edges
2. `ToolContext` - 20 edges
3. `conn` - 20 edges
4. `Ingestor` - 19 edges
5. `Multi-Tenancy (Namespace-per-Tenant Isolation)` - 19 edges
6. `Turn Workflow (Temporal)` - 17 edges
7. `RunReasonActLoop()` - 16 edges
8. `Provider` - 14 edges
9. `ClassifyRequest` - 14 edges
10. `ClassifyResponse` - 14 edges

## Surprising Connections (you probably didn't know these)
- `Grafana Login Page (v12.3.1) Snapshot` --semantically_similar_to--> `Shared Pool Prometheus Metrics Config`  [INFERRED] [semantically similar]
  .playwright-mcp/page-2026-09-03T23-30-13-714Z.yml → deploy/helm/agent-harness-shared/values.yaml
- `SkillSynthesize Learned-Procedure Mechanism` --conceptually_related_to--> `Removed Classify/Lane/Routing/Planning/Skill-Recording Machinery`  [AMBIGUOUS]
  workflows/scenarios/superpowers-b/README.md → docs/components/turn-pipeline.md
- `Multi-Tenant Isolation` --conceptually_related_to--> `Multi-Tenancy (Namespace-per-Tenant Isolation)`  [INFERRED]
  README.md → docs/components/multi-tenancy.md
- `Session Filesystem Mount (+ Permission List)` --references--> `User Input Requests (Mid-Turn Human Interaction)`  [EXTRACTED]
  deploy/helm/agent-harness-tenant/templates/tenant-worker-deployment.yaml → docs/components/user-input.md
- `Tenant Gateway Values Block` --references--> `Component: Budget & Guardrails`  [EXTRACTED]
  deploy/helm/agent-harness-tenant/values.yaml → docs/components/budget-guardrails.md

## Import Cycles
- None detected.

## Hyperedges (group relationships)
- **Agent Harness Architecture Flow (Gateway to Session Coordinator to Turn Workflow)** — readme_gateway, readme_session_coordinator, readme_turn_control_workflow, readme_shared_activities, readme_recursive_subagents, readme_state_workspace_context_architecture [EXTRACTED 1.00]
- **agent-harness-tenant Release Components** — deploy_helm_agent_harness_tenant_chart_postgresql_subchart, deploy_helm_agent_harness_tenant_templates_tenant_worker_deployment_deployment, deploy_helm_agent_harness_tenant_chart_agent_brain_subchart, deploy_helm_agent_harness_tenant_chart_mcp_hub_subchart, deploy_helm_agent_harness_tenant_templates_gateway_deployment_deployment, deploy_helm_agent_harness_tenant_templates_agent_web_deployment_deployment [EXTRACTED 1.00]
- **Gateway Voice Pipeline Components** — deploy_helm_agent_harness_tenant_templates_gateway_deployment_vad_sidecar, deploy_helm_agent_harness_tenant_templates_gateway_deployment_whisperlive_integration, deploy_helm_agent_harness_tenant_templates_gateway_deployment_speech_tts, deploy_helm_agent_harness_tenant_templates_gateway_deployment_discord_bot_tokens [INFERRED 0.85]
- **Discord Voice DSP/Turn-Taking Stack (VAD, EOT, Backchannel, Barge-in, Sanitizer)** — components_gateway_discord_voice_silero_vad, components_gateway_discord_voice_livekit_turn_detector, components_gateway_discord_voice_backchannel, components_gateway_discord_voice_bargein, components_gateway_discord_voice_emoji_sanitizer, components_gateway_discord_voice_text_normalize, components_gateway_discord_voice_filler_ladder [EXTRACTED 1.00]
- **Per-Platform Gateway Kind Docs Implementing Shared gateway.md Contract** — docs_components_gateway, components_gateway_web, components_gateway_discord, components_gateway_discord_voice, components_gateway_mobile, components_gateway_realtime [EXTRACTED 1.00]
- **Core Architecture Doc Series (Parts 1-5)** — docs_01_architecture_overall_topology, docs_02_architecture_temporal_execution, docs_03_architecture_comparison_vs_hermes, docs_04_architecture_orchestrator_vision, docs_05_architecture_domain_control_loops [EXTRACTED 1.00]
- **No-Fallback Principle Applied Across Components** — docs_components_software_engineering_no_fallback_principle, docs_components_proactivity_degradation, docs_components_state_layer_delivered_responses, docs_components_model_registry_no_cross_tier_fallback [INFERRED 0.85]
- **Machinery Removed by the Turn-Pipeline Redesign** — docs_components_turn_pipeline_removed_machinery, docs_components_turn_pipeline_turn_pipeline, docs_components_software_engineering_premise_stale, docs_components_tool_registry_tool_registry [EXTRACTED 1.00]
- **Discord Mid-Turn User-Input Response-Routing Flow** — docs_components_user_input_request_workflow, docs_components_user_input_discord_text_buttons, docs_components_user_input_discord_voice_reuse, docs_components_user_input_custom_id_bug [EXTRACTED 1.00]

## Communities (140 total, 12 thin omitted)

### Community 0 - "Realtime Session Access & Tailing"
Cohesion: 0.06
Nodes (40): AlreadyAnsweredError, HistoryMessage, HistoryTurn, PendingInput, SessionSummary, Once, conn, hub (+32 more)

### Community 1 - "Core Activities: DB & Context"
Cohesion: 0.05
Nodes (37): CompressContextActivity, create_pool(), Pool, Postgres connection-pool access for the activity layer.  Real design: every cont, Create the process-wide connection pool from POSTGRES_* env vars., GetMaxTurnSeqActivity, GetMaxTurnSeq activity — the real body for what coordinator.go's own comment alr, CheckConditionActivity (+29 more)

### Community 2 - "VAD/EOT Sidecar Models"
Cohesion: 0.05
Nodes (30): EndOfTurnModel, ndarray, End-of-turn (turn-taking) classification via LiveKit's turn-detector v1-mini — d, Wraps `livekit.local_inference.EOT` — a native pybind11 object with     no docum, _locate_bundled_model_path(), ndarray, Silero VAD classification, reimplemented against the raw ONNX session directly r, SileroVADModel (+22 more)

### Community 3 - "Discord Voice Delivery & Components"
Cohesion: 0.07
Nodes (39): Decoder, MessageComponent, ReadCloser, Reader, BuildUserInputComponents(), Context, voiceDeliverActivity, Context (+31 more)

### Community 4 - "Architecture Topology & Delivery Decisions"
Cohesion: 0.05
Nodes (46): Short-Polling Delivery Decision (rejected live push), Overall Topology & Design Decisions (Part 1), Hermes AIAgent Class (run_agent.py), Postgres as Shared State Layer (replacing SQLite), Reason-Act-Observe (ReAct) Loop, Hermes Scaling Problem (single-threaded per-session, in-process guard, SQLite serialized writes), Temporal Execution Design (Part 2), Delivery Routing Problem (Outbound Queue Decision) (+38 more)

### Community 5 - "IDs, Claim-Check & Subagent Manifest"
Cohesion: 0.07
Nodes (37): Claim-check store for large tool outputs — closes docs/components/session-filesy, CompressContext activity.  Real design: triggered by the turn workflow's two-tie, activity_id(), Mirrors workflows/internal/ids/ids.go by hand — same scheme, Python side.  Under, {turn_id}:act:{n}" — a plain tool call's fully-qualified activity ID., {turn_id}:sub:{n}" — a subagent child workflow's ID, nested under its     parent, The inverse of `activity_id` — the turn_id (top-level or     subagent-nested) th, subagent_turn_id() (+29 more)

### Community 6 - "LCM Context Assembly & Compaction"
Cohesion: 0.09
Nodes (39): assemble(), Session-wide context assembly — docs/components/context-slot.md, "Resolved: Duti, Session-wide, not turn-scoped — every top-level turn's messages,     ordered by, Returns (conversation, context_tokens): conversation is OpenAI-shaped,     ready, session_messages(), _build_transcript(), compact(), compression_state() (+31 more)

### Community 7 - "VAD Sidecar Protobuf Messages"
Cohesion: 0.07
Nodes (20): ClassifyRequest, ClassifyResponse, UnimplementedVADServer, UnsafeVADServer, VADClient, VADServer, ClientConnInterface, Context (+12 more)

### Community 8 - "Gateway Ingestion & Main"
Cohesion: 0.12
Nodes (31): Ingestor, MessageEvent, discordBotTokens(), envOrDefault(), MetricsHandler, main(), newMetricsHandler(), Client (+23 more)

### Community 9 - "EOT Sidecar gRPC Protobuf"
Cohesion: 0.08
Nodes (20): EOTClient, EOTServer, PredictRequest, PredictResponse, UnimplementedEOTServer, UnsafeEOTServer, _EOT_Predict_Handler(), ClientConnInterface (+12 more)

### Community 10 - "Discord Delivery & Reply Mode"
Cohesion: 0.11
Nodes (21): discordDeliverActivity, discordSendableContent(), discordSplitForLimit(), Context, Message, Pool, Session, ToolCallInput (+13 more)

### Community 11 - "Coordinator & Scenario Scaffolding"
Cohesion: 0.09
Nodes (28): ChildWorkflowFuture, scenario, scenarioModelResponse, scenarioToolCall, WakePayload, Usage, CoordinatorInput, envOrDefault() (+20 more)

### Community 12 - "Clerk Auth & Realtime Handler"
Cohesion: 0.12
Nodes (23): Config, jwksCache, PublicKey, Config, Handler, ResolveScope, Scope, Upgrader (+15 more)

### Community 13 - "Capability Table & Tool Schema"
Cohesion: 0.10
Nodes (26): Capability, Layer, _mint_name(), mint_resolved(), The capability table — docs/components/tool-registry.md, "Resolved: Three-Layer, The model-facing tool schema for a turn: the static capabilities whose     `turn, A valid, turn-unique OpenAI function name for a resolved tool. Prefers     the b, Turn discover_tools's staged `(content, metadata)` rows — `content` =     "{serv (+18 more)

### Community 14 - "Intention Tool Activities"
Cohesion: 0.17
Nodes (25): The session_key prefix of any turn_id (top-level or subagent) — the     same spl, The user-stable scope a standing intention keys on     (docs/components/proactiv, session_key_of(), user_scope_of(), cancel_intention(), _client(), create_intention(), _create_recurring() (+17 more)

### Community 15 - "ModelCall Activity"
Cohesion: 0.11
Nodes (21): ModelCallActivity, ModelCallOutput, ModelCall activity — the reasoning step, and (under the reference-passing contra, Wraps llm.call_model_streaming with this feature's two other real         pieces, Bound-method activity so the Postgres pool and Temporal client     (both created, docs/components/temporal-workflow.md's recursion-termination guard     (LCM/Volt, _validate_subagent_delegation(), Message (+13 more)

### Community 16 - "Shared Workflow Types (Go)"
Cohesion: 0.12
Nodes (23): NextStep, ApprovalGatedCallSpec, CheckConditionInput, CheckConditionResult, FireIntentionInput, InsertMessageInput, IntentionStatus, Message (+15 more)

### Community 17 - "Skill Workflows: Draft Note & Journaling"
Cohesion: 0.14
Nodes (20): SkillWorkflowInput, SkillWorkflowOutput, DraftNoteSkillWorkflow(), Context, Context, JournalingSkillWorkflow(), closeSkillCall(), Context (+12 more)

### Community 18 - "LLM Provider Abstraction"
Cohesion: 0.13
Nodes (13): ABC, AnthropicProvider, Anthropic Messages API provider — docs/components/model-registry.md's "anthropic, Provider, Provider ABC — the shape every provider implementation implements.  Internal con, Full agent-conversation call, non-streaming. Returns         RealModelResult (fr, Streaming counterpart to call_model. on_chunk is an async         callable invok, Simple system+user prompt → text. No tools, no streaming.         Used by lcm.co (+5 more)

### Community 19 - "Exploration Summary & Claim-Check Helpers"
Cohesion: 0.12
Nodes (21): _preview(), If data fits inline, returns {"inline": text}. Otherwise writes it     to `.clai, `{turn_id}:act:{n}` contains colons — legal on POSIX but ugly and     a footgun, Head+tail split. Falls back to empty tail if the content is     shorter than hea, _sanitize_id(), store_if_large(), _describe_json(), _is_binary() (+13 more)

### Community 20 - "LLM Integration & Report Status"
Cohesion: 0.13
Nodes (13): Real LLM integration for ModelCall (activities/activities/model_call.py).  This, RealModelResult, _openai_conversation_to_anthropic(), _openai_tools_to_anthropic(), Unwrap OpenAI's {type:"function", function:{...}} envelope into     Anthropic's, Returns (system_prompt, messages). Pulls the system message out     of `messages, parse_report_status(), Parsed `report_status` payload. `status` empty ⇒ the model didn't call     it th (+5 more)

### Community 21 - "Proactivity & Project Authoring Docs"
Cohesion: 0.11
Nodes (22): Proactivity Degradation (No Fallback), Intention Fire Path (Wake Signal → Deciding Turn), Genesis Daily-Review Intention, IntentionWorkflow, Proactivity — Intentions, IntentionUser/IntentionKind/IntentionState Search Attributes, code_task (delegate_claude_code, project-bound), deploy Activity (Build/Rollout/Verify/Rollback) (+14 more)

### Community 22 - "Turn Workflow Core Loop"
Cohesion: 0.19
Nodes (21): Future, awaitModelCallWithStreaming(), CompressContextWorkflow(), connectionDeliveryChunkActivity(), deliverWedgedFallback(), deliveryTaskQueue(), deliveryToolActivity(), dispatchSubagentManifests() (+13 more)

### Community 23 - "Agent-Brain Retain Client"
Cohesion: 0.13
Nodes (17): AgentBrainCallError, AgentBrainNotConfiguredError, call_retain_tool(), ensure_persona_mental_model(), Any, RuntimeError, Thin async client for agent-brain's retain MCP server (docs/components/ memory-s, bank_id sent on every retain-server call — one bank per tenant, reusing the (+9 more)

### Community 24 - "Tenant Helm Chart & Jobs"
Cohesion: 0.15
Nodes (19): agent-harness-tenant Chart (per-tenant stack), mcp-hub Subchart Dependency, Rationale: Postgres, agent-brain, mcp-hub deployed one-release-per-tenant, Bitnami PostgreSQL Subchart Dependency, Tenant Worker/Gateway Shared ConfigMap, Postgres Init-Roles Job (agentbrain/mcphub roles+DBs), Migrations ConfigMap (SQL files), Rationale: schema_migrations table replaces single-table-existence check for idempotent re-runs (+11 more)

### Community 25 - "Session Directory Leases"
Cohesion: 0.14
Nodes (17): is_claim_check_dir(), Predicate for subagent_manifest.py / merge_subagent_output — used to     prune t, Maps a turn_id to its working directory on the session filesystem PV,     per do, session_fs_path(), acquire_or_renew(), Pool, Session-directory leases — coordinates concurrent tool-call access to the shared, Acquire `path` for `holder_id`, or renew it if already held by the same     hold (+9 more)

### Community 26 - "Discord Bot Core"
Cohesion: 0.17
Nodes (11): User, discordMentionsUser(), Context, Bot, MessageCreate, Session, isPersonalDM(), discordVoiceMessageContent() (+3 more)

### Community 27 - "Session Coordinator & State Layer Docs"
Cohesion: 0.15
Nodes (18): GetMaxTurnSeq Activity, WorkflowIDReusePolicy = AllowDuplicate, Session Coordinator, turn_seq Hardcoded-to-0 Bug Fix, delivered_responses Idempotency Ledger (Deliberately Not Pruned), messages Table, Read/Write Split Per Writer, Postgres Schema (Temporal-ID-Borrowed Primary Keys) (+10 more)

### Community 28 - "Discord Gateway Deployment & Voice Docs"
Cohesion: 0.17
Nodes (17): Gateway: Discord, Gateway: Discord Voice, Kokoro Sample-Rate Conversion Bug (24kHz native, upsample2xPCM fix), Gateway Deployment, Discord Bot Tokens (multi-bot), Speech STT/TTS Integration (Kokoro via litellm-service), Rationale: VAD sidecar colocated in-pod due to ~50x/sec call frequency, VAD Sidecar Container (Silero VAD) (+9 more)

### Community 29 - "Shared Pool & Multi-Tenancy Docs"
Cohesion: 0.16
Nodes (16): agent-harness Shared Chart (loop-worker pool), Rationale: shared loop-worker pool deployed once per cluster, not per tenant, ModelCall/ToolCall/Persist/Deliver/CompressContext Activities, Shared Loop-Worker Pool Deployment Notes, Shared Pool Temporal Config (address/namespaces/taskQueue), Tenant Stack Deployment Notes, Tenant Temporal Config (namespace/taskQueue), Per-Tenant Value Override Convention (+8 more)

### Community 30 - "Voice EOT & Silero VAD Client"
Cohesion: 0.19
Nodes (11): sileroVAD, sileroVADClient, HealthClient, downmixResample16kMonoInt16(), bytesToF32(), downmixResample(), f32ToBytes(), ClientConn (+3 more)

### Community 31 - "Shell-Hub Tool Discovery"
Cohesion: 0.20
Nodes (14): _build_catalog(), _build_embedder(), _describe_command(), _discover_path_commands(), _fts_safe(), init(), _is_substantial(), shell-hub — docs/components/tool-registry.md, "Resolved: Native-Tool Discovery — (+6 more)

### Community 32 - "ToolCall Activity"
Cohesion: 0.18
Nodes (9): DenyToolCallActivity, ToolCallInput, ToolCallOutput, docs/components/user-input.md — ApprovalGatedToolCallWorkflow's own     exit pat, ToolCallActivity, ToolCall's only input — it reads its own arguments from Postgres via     this ID, ToolCall's only output — status, not result/reason/side_effect. Those     stay i, ToolCallInput (+1 more)

### Community 33 - "Reason-Act Loop Types & Dispatch"
Cohesion: 0.24
Nodes (14): Channel, ToolCallRef, TurnResult, deliveryInterruptSource, RunReasonActLoopInput, RunReasonActLoopResult, SignalPayload, compressionState() (+6 more)

### Community 34 - "Discord Voice Connection Lifecycle"
Cohesion: 0.32
Nodes (7): Context, Voice, InteractionCreate, Session, isBackchannelOnly(), newSileroVADClient(), vadSidecarURL()

### Community 35 - "WhisperLive Streaming STT Client"
Cohesion: 0.22
Nodes (7): WhisperLiveSession, wlSegment, wlServerMessage, Conn, Mutex, NewWhisperLiveSession(), WhisperLiveURL()

### Community 36 - "Model Registry & Provider Cache"
Cohesion: 0.21
Nodes (11): get_provider(), Provider cache keyed by (provider, base_url, api_key).  docs/components/model-re, Returns a cached Provider for this tier's config triple. Raises     a clear erro, _cost_per_token(), default_hint(), escalate(), ModelConfig, Model Registry — docs/components/model-registry.md. Modality x Tier structure (r (+3 more)

### Community 37 - "Agent-Web & Agent-Brain Deploy"
Cohesion: 0.18
Nodes (13): agent-brain Subchart Dependency, agent-web Deployment, agent-web Service, Agent-Brain Retain MCP Integration, Rationale: agentBrain adminSecret defaulted empty to avoid insecure placeholder, Tenant agent-brain Subchart Values, Session-Wide Scope Resolution (not per-turn), Component: Dreaming (Session Consolidation) (+5 more)

### Community 38 - "Voice Filler-Phrase Player"
Cohesion: 0.26
Nodes (9): voiceFillerCache, voiceFillerPlayer, voiceProgressPhrase, fillerEnabled(), Context, Duration, VoiceConnection, newVoiceFillerPlayer() (+1 more)

### Community 39 - "Discord Voice State & Commands"
Cohesion: 0.20
Nodes (11): ApplicationCommand, CancelFunc, activeVoiceConnection, voiceState, Worker, Session, registerCommands(), Commands() (+3 more)

### Community 40 - "LLM Tier Config & No-Fallback"
Cohesion: 0.20
Nodes (12): LLM Tier Config Validation (fast/medium/expert), Rationale: every LLM tier owns its own provider identity, no cross-tier fallback, Tenant LLM Tiers Values, declare_next_step_hint (superseded selection mechanism), failTurn — Fallback Beyond Escalate-on-Retry, Model Registry, No Cross-Tier Fallback / No Shared Provider Defaults, Model Pricing Table (input/output/cached-input rates) (+4 more)

### Community 41 - "First-Party Realtime Gateway Docs"
Cohesion: 0.20
Nodes (11): Shared Client Gateway: Design and Delivery Plan (First-Party), Component: Native Mobile Gateway, Client-Owned Resume Cursor (turn_seq), Mobile Session Model (one per user+platform, fanned out), Component: First-Party Realtime Gateway, Structured tool_call Frames (Web capability), Shared Realtime Transport (auth, catch-up, tail, delta frames), Gateway: Web (First-Party Browser Chat — agent-web) (+3 more)

### Community 42 - "Mobile/Web Presence & Session Docs"
Cohesion: 0.18
Nodes (11): device_id/speaker_id Separation Fix, KeepAlive Mechanism (coordinator lifetime tied to connection), Cross-Replica Presence (mobile_presence table), Multi-Session Support (user-initiated branch, websession.go), agent-brain Temporal Schedules (mining-entities/facts/rules/generalize, prototypes, emu-construction, emu-lifecycle), Dreaming's Batch-Job Role Superseded by agent-brain Consolidation Pipeline, Resolved Inbound Flow (dedup + SignalWithStart), Generic MessageEvent Shape (+3 more)

### Community 43 - "Web Session & Context-Slot Docs"
Cohesion: 0.20
Nodes (11): One Continuous Session Per User (no reset, no threads), context_summaries Table Schema, folded_into Losslessness Bug Fix (DELETE→UPDATE), lcm_grep covered_by_summary_id Nested-Fold Bug, Memory-Access Tools (lcm_grep/lcm_describe/lcm_expand), Soft/Hard Compression Trigger Lossiness Bug (fires unconditionally fix), Hierarchical Summary DAG (leaf/condensed), Three-Level Escalation Compaction (+3 more)

### Community 44 - "Discord User-Input Interactions"
Cohesion: 0.27
Nodes (7): Interaction, Context, Bot, InteractionCreate, Message, Session, optionLabelFromComponents()

### Community 45 - "Recursion Guard & Future Work"
Cohesion: 0.22
Nodes (11): Recursion Termination Guard (delegated_scope/kept_work), spawn_subagent Regression Bugs (caller_is_subagent NameError, has_tool_calls), Model Ends Turn With No Content (Root-Caused, Fix Deferred), Future Work & Explorations, Do We Really Need Temporal? (Resolved: Keep It), Real-Time/Voice Interaction (Open), OpenClaw (Project), Tracker: OpenClaw (+3 more)

### Community 46 - "Realtime Wire Protocol Frames"
Cohesion: 0.20
Nodes (10): askUserFrame, deltaFrame, errorFrame, messageFrame, resumedFrame, toolCallFrame, turnEndFrame, turnStartFrame (+2 more)

### Community 47 - "Intention Workflow Tests"
Cohesion: 0.38
Nodes (10): FireIntentionInput, T, TestWorkflowEnvironment, mockFire(), TestIntentionWorkflow_ConditionPollsThenFires(), TestIntentionWorkflow_InactivityResetKeepsArmed(), TestIntentionWorkflow_SnoozeDelaysFire(), TestIntentionWorkflow_TimeFiresOnce() (+2 more)

### Community 48 - "Skill Call Activities"
Cohesion: 0.20
Nodes (5): CloseSkillCallActivity, A skill workflow's two activities — docs/05-architecture-domain-control-loops.md, A skill workflow's only way to see its real input — SkillWorkflowInput     on th, Closes out a skill's tool_calls row in exactly one of the same three     termina, ReadSkillCallArgumentsActivity

### Community 49 - "Skill Registry & Discovery"
Cohesion: 0.24
Nodes (8): _build_embedder(), _fts_safe(), init(), skill-hub — docs/05-architecture-domain-control-loops.md, docs/components/ turn-, Returns matched skill registry entries: {name, description,     input_schema, wo, Called once at worker startup. No-op if EMBEDDING_BASE_URL isn't set     (same g, search(), skills.py — docs/05-architecture-domain-control-loops.md, docs/components/ turn-

### Community 50 - "Voice DSP: Barge-In, Filler, VAD"
Cohesion: 0.20
Nodes (10): Per-Channel Reply Mode (/mode reply:voice|text), Backchannel Detection Mechanism (voice_backchannel.go), Fast-Path Barge-In (voice_bargein.go), Reactive Filler-Phrase Ladder (voice_filler_player.go), Gated Streaming STT Feed (WhisperLive audio gating fix), GPU Saturation Incident (unconditional WhisperLive feed, OOM), LiveKit turn-detector v1-mini (End-of-Turn Model), Voice-Message Attachment Transcription (+2 more)

### Community 51 - "Voice Latency Tracking"
Cohesion: 0.24
Nodes (5): voiceLatencyTracker, Duration, Mutex, Time, newVoiceLatencyTracker()

### Community 52 - "Regression Suite & Registry Docs"
Cohesion: 0.20
Nodes (10): Escalate-on-Retry (fast→medium→expert per Temporal attempt), Postgres-Backed Session Directory Leases, session_filesystem_leases Table, mcp-hub-Mediated Tool Tier, Deep Conversation Suite, Deep-Conversation Main Chained Session, run_all.sh Regression Runner, Scenarios — Regression Suite (+2 more)

### Community 53 - "Web Gateway Auth"
Cohesion: 0.24
Nodes (7): contextKey, Config, Context, Handler, requireClerkAuth(), userIDFromContext(), ServeMux

### Community 54 - "Web Cancel & Sessions Handlers"
Cohesion: 0.20
Nodes (8): Request, ResponseWriter, Handler, Request, ResponseWriter, Handler, ResponseWriter, writeJSON()

### Community 55 - "MCP-Hub Client"
Cohesion: 0.33
Nodes (8): call_tool(), _mcp_url(), McpHubCallError, McpHubNotConfiguredError, Any, RuntimeError, Thin async client for mcp-hub's MCP endpoint (docs/components/tool-registry.md,, Calls one mcp-hub MCP tool (search_tools or call_tool) and returns its     parse

### Community 56 - "Session Filesystem & PVC Docs"
Cohesion: 0.25
Nodes (9): Tenant Session-Filesystem PVC (ReadWriteMany), Tenant Session-Filesystem Volume Values, Reference-Passing Contract (Multi-Tenancy Summary), Claim-Check Large-Payload Routing (PV), Type-Aware Exploration Summary, Multi-Tenant Access Scoping (PV per Tenant), Session Filesystem (Shared Mount + Postgres-Backed Leases), Subagent Merge-Back Mechanics (+1 more)

### Community 57 - "Voice EOT Client"
Cohesion: 0.25
Nodes (6): eotClient, errStr, ClientConn, Context, Duration, newEOTClient()

### Community 58 - "Voice Barge-In Mechanism"
Cohesion: 0.25
Nodes (3): voiceBargeIn, Mutex, newVoiceBargeIn()

### Community 59 - "User-Input Request Workflow"
Cohesion: 0.39
Nodes (8): UserInputRequestWorkflowInput, connectionInterimDeliveryActivity(), dispatchInterimDelivery(), Context, Duration, Logger, markDenied(), UserInputRequestWorkflow()

### Community 60 - "Loop-Worker Main"
Cohesion: 0.47
Nodes (8): envOrDefault(), Context, MetricsHandler, main(), namespaces(), newMetricsHandler(), retryForNamespace(), runForNamespace()

### Community 61 - "InsertMessage Activity"
Cohesion: 0.29
Nodes (5): InsertMessageActivity, InsertMessage activity — the one place content still crosses an activity input b, InsertMessageInput, Input for the message-insert activity — the one place content still     crosses, InsertMessageInput

### Community 62 - "Skill Reasoning Outcome"
Cohesion: 0.29
Nodes (5): ReasoningTurnOutcome, SummarizeReasoningTurn — docs/05-architecture-domain-control-loops.md.  A skill', SummarizeReasoningTurnActivity, skills.RunReasoningTurn's result — what SummarizeReasoningTurn distills     a sk, ReasoningTurnOutcome

### Community 63 - "Energy VAD & Speaker Buffer"
Cohesion: 0.29
Nodes (4): energyVAD, speakerBuffer, voiceActivityDetector, newEnergyVAD()

### Community 64 - "Interrupt Model & Button Routing Docs"
Cohesion: 0.25
Nodes (8): Interrupt Model, custom_id Colon-Split Bug (Every Real Button Click Broken), Discord Text Button Response-Routing, Discord Voice Response-Routing (Reuses Text Button Flow), Mid-Turn Interim Delivery (Push Half), PERMISSION_LIST (Decoupled Permission Gating), UserInputRequestWorkflow, User Input Requests (Mid-Turn Human Interaction)

### Community 65 - "Gateway Lease Manager"
Cohesion: 0.36
Nodes (5): Manager, Context, Duration, Pool, NewManager()

### Community 66 - "Mobile Briefings"
Cohesion: 0.29
Nodes (6): briefing, briefingResponse, Time, summaryForBriefing(), T, TestSummaryForBriefing()

### Community 67 - "Mobile Briefing Handlers"
Cohesion: 0.57
Nodes (4): Context, Handler, Request, ResponseWriter

### Community 68 - "Intention Workflow Core"
Cohesion: 0.29
Nodes (7): ProbeSpec, IntentionInput, IntentionReviseSignal, Duration, Time, Context, IntentionWorkflow()

### Community 69 - "Permission Gating"
Cohesion: 0.33
Nodes (6): docs/components/user-input.md — the {server, tool} identity being     gated depe, _resolve_gating(), _load_permission_list(), PermissionRule, Permission gating — docs/components/user-input.md, "Resolved: Permission Gating, requires_approval()

### Community 70 - "StatusPing Activity"
Cohesion: 0.38
Nodes (3): _describe_call(), StatusPing activity — docs/components/turn-pipeline.md, "Progress watchdog".  Tu, StatusPingActivity

### Community 71 - "README Architecture Summary & Shared Deploy"
Cohesion: 0.33
Nodes (7): Shared Loop-Worker ConfigMap, Loop-Worker Deployment (Session Coordinator + Turn Workflow), Gateway, Recursive Subagents, Session Coordinator, State, Workspace & Context Architecture, Turn / Control Workflow

### Community 72 - "Discord Voice Client Setup"
Cohesion: 0.48
Nodes (5): Client, Context, Voice, Pool, New()

### Community 73 - "Discord Bot Setup"
Cohesion: 0.43
Nodes (6): Voice, Client, Context, Bot, Pool, New()

### Community 74 - "Session Key Derivation"
Cohesion: 0.43
Nodes (5): DiscordThreadRootFromSessionKey(), SessionKeyFor(), T, TestDiscordThreadRootFromSessionKey(), TestSessionKeyFor()

### Community 75 - "Web Session ID Mapping"
Cohesion: 0.38
Nodes (5): sessionDiscriminator(), sessionIDFromKey(), T, TestSessionDiscriminator(), TestSessionIDFromKeyUsesPlatformNamespace()

### Community 76 - "Voice Text Normalization & Prompt Bug"
Cohesion: 0.40
Nodes (6): Emoji/Markdown-to-Speech Sanitizer (voice_text_sanitize.go), Platform-Specific Voice System Prompt (voiceSystemPromptText), Voice Formatting Plan-Inspired Text Normalization (voice_text_normalize.go), Vapi Voice Formatting Plan (Industry Reference), Voice/Text Mode Per-Message (messages.mode), Voice System Prompt Missing Provisioning Section Bug (2026-09-24)

### Community 77 - "Voice Lifecycle State Machine"
Cohesion: 0.53
Nodes (4): voiceLifecycle, voiceLifecycleState, Mutex, newVoiceLifecycle()

### Community 79 - "Superpowers B1 Scenario Test"
Cohesion: 0.53
Nodes (4): fail(), ok(), b1-follow-external.expect.sh script, warn()

### Community 80 - "Superpowers B2 Scenario Test"
Cohesion: 0.53
Nodes (4): fail(), ok(), b2-learned-retrieval.expect.sh script, warn()

### Community 81 - "Context-Slot & Budget-Guardrails Concepts"
Cohesion: 0.40
Nodes (5): Rule-Driven Controlled Stop (Future Scope), Component: Context Slot (Short-Term Memory), Claim-Check Storage + Exploration Summary (tool-output bounding), LCM (Lossless Context Management, Ehrlich & Blackman 2026), Genesis Context Injection (SeedChildSessionContextActivity)

### Community 82 - "Web Poll Response Types"
Cohesion: 0.60
Nodes (4): pendingInput, polledTurn, pollResponse, RawMessage

### Community 83 - "Anthropic-Basic Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), anthropic-basic.expect.sh script

### Community 84 - "Ask-User-Followup Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), ask-user-followup.expect.sh script

### Community 85 - "Blocked-Terminal Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), blocked-terminal.expect.sh script

### Community 86 - "Ceiling-Raise Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), ceiling-raise.expect.sh script

### Community 87 - "Claim-Check-Large-Output Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), claim-check-large-output.expect.sh script

### Community 88 - "Discover-Skill-Dispatch Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), discover-skill-dispatch.expect.sh script

### Community 89 - "Done-With-Empty-Content Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), done-with-empty-content.expect.sh script

### Community 90 - "Done-With-Tool-Calls Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), done-with-tool-calls.expect.sh script

### Community 91 - "Exploration-Summary-CSV Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), exploration-summary-csv.expect.sh script

### Community 92 - "Exploration-Summary-JSON Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), exploration-summary-json.expect.sh script

### Community 93 - "Exploration-Summary-Text Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), exploration-summary-text.expect.sh script

### Community 94 - "Happy-Path Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), happy-path.expect.sh script

### Community 95 - "Interrupt-Followup Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), interrupt-followup.expect.sh script

### Community 96 - "LCM-Grep-Nested-Fold Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), lcm-grep-nested-fold.expect.sh script

### Community 97 - "LCM-Retrieval Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), lcm-retrieval.expect.sh script

### Community 98 - "Lite-Simple-Task Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), lite-simple-task.expect.sh script

### Community 99 - "Max-Iterations Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), max-iterations.expect.sh script

### Community 100 - "No-Progress-Failure Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), no-progress-failure.expect.sh script

### Community 101 - "No-Progress-Guard Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), no-progress-guard.expect.sh script

### Community 102 - "Parallel-Subagents Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), parallel-subagents.expect.sh script

### Community 103 - "Real-Assembly Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), real-assembly.expect.sh script

### Community 104 - "Resolved-Tool-Dispatch Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), resolved-tool-dispatch.expect.sh script

### Community 105 - "Shell-Exec-Basic Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), shell-exec-basic.expect.sh script

### Community 106 - "Shell-Exec-Slow Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), shell-exec-slow.expect.sh script

### Community 107 - "Skill-Interrupt-Followup Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), skill-interrupt-followup.expect.sh script

### Community 108 - "Spawn-Subagent-Nested-Rejected Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), spawn-subagent-nested-rejected.expect.sh script

### Community 109 - "Spawn-Subagent-Nested-Valid Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), spawn-subagent-nested-valid.expect.sh script

### Community 110 - "Subagent-Full-Agent Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), subagent-full-agent.expect.sh script

### Community 111 - "Subagent-Spawn Scenario Test"
Cohesion: 0.60
Nodes (3): fail(), ok(), subagent-spawn.expect.sh script

### Community 112 - "Cancel Primitive Test Script"
Cohesion: 0.60
Nodes (3): fail(), ok(), test_cancel.sh script

### Community 113 - "Sentence Boundary Segmenter"
Cohesion: 0.50
Nodes (3): find_boundary(), Real sentence-boundary detection for ModelCall streaming (docs/components/gatewa, Return the index just past the first confident sentence boundary in     buffer (

### Community 114 - "Discord Ambient Message Buffer"
Cohesion: 0.50
Nodes (4): discord_ambient_messages Buffer Table, Non-Content MessageCreate Event Filtering (by m.Type), Reply-Chain Resolution Through Bot's Own Messages Bug Fix, Response Scope: Ingest Everything, Respond Only When Addressed

### Community 115 - "Cancel Primitive & Shared Access Docs"
Cohesion: 0.50
Nodes (4): CancelSignalName Primitive (stop-turn), core.access.go Shared Handlers (AnswerUserInput, CancelActiveTurn), Shared Session Access Principle (clients access, don't define, sessions), Server-Owned Scope Resolver (per platform adapter)

### Community 116 - "Progress Narration & Wire Protocol Docs"
Cohesion: 0.50
Nodes (4): Event-Driven Progress Narration, Status Pings as Delta Frames (progress field), Mobile WebSocket Wire Protocol (auth/message/answer/resume frames), Clerk Auth Reuse (agent-web integration)

### Community 117 - "Mobile Live Smoke Test"
Cohesion: 0.50
Nodes (3): errorWireFrame, T, TestLiveSmoke()

### Community 118 - "Web Poll Handler"
Cohesion: 0.50
Nodes (3): Request, ResponseWriter, Handler

### Community 119 - "Web Respond Handler"
Cohesion: 0.50
Nodes (3): Request, ResponseWriter, Handler

### Community 120 - "Web Send Handler"
Cohesion: 0.50
Nodes (3): Request, ResponseWriter, Handler

### Community 121 - "Tool Timing Tiers"
Cohesion: 0.67
Nodes (3): toolTiming, Duration, toolTimingFor()

### Community 123 - "Grafana Snapshot & Metrics Config"
Cohesion: 0.67
Nodes (3): Shared Pool Prometheus Metrics Config, Component: Budget & Guardrails, Grafana Login Page (v12.3.1) Snapshot

### Community 124 - "Gateway-Per-Platform Topology Doc"
Cohesion: 1.00
Nodes (3): One Gateway Type Per Platform Topology Decision, Component: Gateway (Per-Platform Ingestion & Delivery), Per-Tenant Gateway Deployment (one process per tenant, all platforms via goroutines)

### Community 125 - "Draft-Note Skill & UseSkill Field Doc"
Cohesion: 0.67
Nodes (3): draft_note Skill (harness-validation skill), Skill Discovery and Invocation (Static, Always-On Capability, not discover_tools shape), UseSkill Field (ToolCallRef Collapsed IsSkill+ResolvedWorkflowType)

## Ambiguous Edges - Review These
- `Interrupt-and-Signal (Cooperative Cancellation)` → `Panic Handling (Defense-in-Depth, not load-bearing isolation)`  [AMBIGUOUS]
  docs/components/activities-outbound-delivery.md · relation: references
- `Removed Classify/Lane/Routing/Planning/Skill-Recording Machinery` → `SkillSynthesize Learned-Procedure Mechanism`  [AMBIGUOUS]
  workflows/scenarios/superpowers-b/README.md · relation: conceptually_related_to

## Knowledge Gaps
- **98 isolated node(s):** `ToolSpec`, `Message`, `UserInputOption`, `UserInputResponse`, `ProbeSpec` (+93 more)
  These have ≤1 connection - possible missing edges or undocumented components.
- **12 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **What is the exact relationship between `Interrupt-and-Signal (Cooperative Cancellation)` and `Panic Handling (Defense-in-Depth, not load-bearing isolation)`?**
  _Edge tagged AMBIGUOUS (relation: references) - confidence is low._
- **What is the exact relationship between `Removed Classify/Lane/Routing/Planning/Skill-Recording Machinery` and `SkillSynthesize Learned-Procedure Mechanism`?**
  _Edge tagged AMBIGUOUS (relation: conceptually_related_to) - confidence is low._
- **Why does `main()` connect `Core Activities: DB & Context` to `ToolCall Activity`, `StatusPing Activity`, `Discord Voice State & Commands`, `ModelCall Activity`, `Skill Call Activities`, `Skill Registry & Discovery`, `Agent-Brain Retain Client`, `InsertMessage Activity`, `Skill Reasoning Outcome`, `Shell-Hub Tool Discovery`?**
  _High betweenness centrality (0.198) - this node is a cross-community bridge._
- **Why does `Ingestor` connect `Gateway Ingestion & Main` to `Discord Voice Client Setup`, `Discord Bot Setup`, `Clerk Auth & Realtime Handler`?**
  _High betweenness centrality (0.091) - this node is a cross-community bridge._
- **Are the 20 inferred relationships involving `ValueError` (e.g. with `session_fs_path()` and `session_key_of()`) actually correct?**
  _`ValueError` has 20 INFERRED edges - model-reasoned connections that need verification._
- **Are the 2 inferred relationships involving `ToolContext` (e.g. with `DenyToolCallActivity` and `ToolCallActivity`) actually correct?**
  _`ToolContext` has 2 INFERRED edges - model-reasoned connections that need verification._
- **What connects `ToolSpec`, `Message`, `UserInputOption` to the rest of the system?**
  _98 weakly-connected nodes found - possible documentation gaps or missing edges._