"""shell-hub — docs/components/tool-registry.md, "Resolved: Native-Tool
Discovery — shell-hub (in-process, zvec)". A small, local, in-process
registry making locally-available CLI capabilities discoverable through
search_tools, without routing shell_exec's actual invocation through
mcp-hub — shell_exec's cancellation/heartbeat contract requires it to stay
a directly-dispatched native Temporal activity (see that doc section for
why routing it through mcp-hub or making mcp-hub itself Temporal-aware were
both considered and rejected).

CATALOG is auto-populated at startup by scanning $PATH for real, executable
commands — not hand-maintained. No entry is fabricated: whatever's
discoverable is exactly whatever's actually installed in the image, nothing
more. Filtered against _COMMON_EXCLUDE (widely-known coreutils/shell
builtins — ls, grep, cat, etc.) since indexing those adds embedding-cost
noise for zero discovery value: any capable model already knows they exist
from training and would call shell_exec directly, without needing
search_tools to surface them — shell-hub's actual value is surfacing
capabilities the model wouldn't otherwise know to reach for. Descriptions
come from each command's own `--help` output (a safe, informational,
side-effect-free convention nearly every real CLI supports) — this is
running arbitrary discovered binaries' --help, which is only an acceptable
assumption because the tenant-worker image is a controlled build artifact,
not an arbitrary/untrusted host; each call is time-bounded and run
concurrently at startup, not serially.

zvec (https://github.com/alibaba/zvec) — genuinely in-process (a local file
path, no server, no network port), hybrid vector+FTS search fused via RRF.
Vector search is necessary, not FTS alone: search_tools receives vague
natural-language task descriptions ("check my inbox"), not literal tool
names — verified against mcp-hub's own real search_tools implementation,
which does the same (embeds the query) for the identical reason.

Built once at tenant-worker startup (init(), called from tenant_worker.py) —
zvec.Collection and the embedding client are both expensive, stateful
resources, same category as the Postgres pool/OpenAI client already
constructed once there, not per-call. Discovery + --help + embedding all run
once per worker process start, not per search() call.
"""

from __future__ import annotations

import asyncio
import logging
import hashlib
import os
import shutil

import zvec

logger = logging.getLogger(__name__)

_COLLECTION_PATH = "/tmp/agent-harness-shell-hub"
_HELP_TIMEOUT_SECONDS = 2.0
_MAX_DISCOVERED_COMMANDS = 1000

# Small, hand-picked, deliberately not exhaustive — the most obvious
# coreutils/shell builtins. Not trying to catch everything a base image
# ships (that was tried — a ~360-entry hardcoded snapshot of
# python:3.12-slim's own PATH — and still didn't produce a clean catalog,
# since pip-installed *dependencies* also dump their own console-script
# entry points on PATH regardless of base-image filtering, e.g. this
# project's own numpy-config/jsonschema/httpx2 showing up as
# transitive-dependency noise). Not worth chasing further: the search step
# itself already provides the real relevance filtering — an irrelevant
# entry just won't rank for a real query, so a little catalog noise costs
# nothing at query time, unlike an ever-growing, image-tag-coupled exclude
# list costs real maintenance.
_COMMON_EXCLUDE = {
    "sh", "bash", "zsh", "dash", "ls", "cat", "grep", "egrep", "fgrep", "awk",
    "sed", "echo", "cd", "pwd", "cp", "mv", "rm", "mkdir", "rmdir", "touch",
    "chmod", "chown", "chgrp", "kill", "ps", "true", "false", "test", "[",
    "printf", "sort", "uniq", "head", "tail", "wc", "cut", "tr", "find",
    "xargs", "tar", "gzip", "gunzip", "zcat", "which", "env", "export",
    "source", "alias", "unalias", "type", "hash", "ln", "readlink", "dirname",
    "basename", "date", "sleep", "wait", "jobs", "bg", "fg", "trap", "set",
    "unset", "read", "eval", "exec", "exit", "return", "shift", "getopts",
    "diff", "patch", "make", "ar", "strip", "nm", "objdump", "ldd", "ldconfig",
    # Python/pip's own launchers — the image's own runtime, not a discoverable
    # "capability" in the sense shell-hub is for.
    "python3", "python", "pip", "pip3",
}

# Manual overrides/additions, keyed by command name — {"description": str}.
# Auto-discovery is the primary mechanism; use this only to hand-improve a
# specific tool's --help-derived description, or add one --help can't
# describe well. Empty by default — nothing hand-seeded.
MANUAL_OVERRIDES: dict[str, str] = {}

_SHELL_EXEC_INPUT_SCHEMA = {
    "type": "object",
    "properties": {"command": {"type": "string", "description": "The shell command to run."}},
    "required": ["command"],
}

_embedder: "zvec.OpenAIDenseEmbedding | None" = None
_collection: "zvec.Collection | None" = None


def _discover_path_commands() -> list[str]:
    """Real $PATH scan — every executable file found, deduped by name
    (first-in-PATH wins, matching real shell resolution order), filtered
    against _COMMON_EXCLUDE. Genuine auto-discovery: this list is exactly
    what's actually on the image, not asserted. Some noise (base-image
    system tools, transitive-dependency console scripts) survives this
    filter — see _COMMON_EXCLUDE's own comment on why that's an accepted
    tradeoff, not a gap to keep closing.

    Capped at _MAX_DISCOVERED_COMMANDS — found necessary by testing, not
    assumed: a real dev machine's $PATH (multiple language toolchains,
    every pip package's own CLI entry point) produced 1700+ results, which
    would mean that many concurrent --help subprocess spawns at startup.
    The tenant-worker's actual deployed image (python:3.12-slim) is far
    more minimal, but this cap is real defensive engineering against a
    bloated PATH regardless, not a hypothetical concern."""
    seen: set[str] = set()
    names: list[str] = []
    for directory in os.environ.get("PATH", "").split(os.pathsep):
        if not directory:
            continue
        try:
            entries = os.listdir(directory)
        except OSError:
            continue
        for name in entries:
            if name in seen or name in _COMMON_EXCLUDE:
                continue
            full_path = os.path.join(directory, name)
            try:
                if os.path.isfile(full_path) and os.access(full_path, os.X_OK):
                    seen.add(name)
                    names.append(name)
            except OSError:
                continue
    if len(names) > _MAX_DISCOVERED_COMMANDS:
        logger.warning(
            "shell_hub: $PATH scan found %d commands, capping at %d — "
            "the excess are silently dropped, not indexed",
            len(names),
            _MAX_DISCOVERED_COMMANDS,
        )
        names = names[:_MAX_DISCOVERED_COMMANDS]
    return names


async def _describe_command(name: str) -> str:
    """Best-effort one-line description via `<name> --help`. Falls back to
    just the bare name if --help fails, times out, or produces nothing
    usable — a command with no extractable description still gets indexed
    (under its bare name), it just won't semantically match much beyond
    literal name mentions."""
    try:
        proc = await asyncio.create_subprocess_exec(
            name,
            "--help",
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            stdin=asyncio.subprocess.DEVNULL,
        )
        stdout, stderr = await asyncio.wait_for(proc.communicate(), timeout=_HELP_TIMEOUT_SECONDS)
        text = (stdout or stderr).decode("utf-8", errors="replace")
        first_line = next((line.strip() for line in text.splitlines() if line.strip()), "")
        if first_line:
            return f"{name}: {first_line}"
    except (asyncio.TimeoutError, OSError):
        pass
    except Exception:  # noqa: BLE001 - a misbehaving discovered binary must never break startup
        logger.warning("shell_hub: --help failed unexpectedly for %r", name, exc_info=True)
    return name


async def _build_catalog() -> list[dict[str, str]]:
    names = _discover_path_commands()
    descriptions = await asyncio.gather(*(_describe_command(name) for name in names))
    catalog = [
        {"name": name, "description": MANUAL_OVERRIDES.get(name, description)}
        for name, description in zip(names, descriptions)
    ]
    for name, description in MANUAL_OVERRIDES.items():
        if name not in names:
            catalog.append({"name": name, "description": description})
    return catalog


def _build_embedder() -> "zvec.OpenAIDenseEmbedding | None":
    base_url = os.environ.get("EMBEDDING_BASE_URL", "")
    if not base_url:
        return None
    return zvec.OpenAIDenseEmbedding(
        model=os.environ.get("EMBEDDING_MODEL", "bge-m3"),
        dimension=int(os.environ.get("EMBEDDING_DIM", "1024")),
        api_key=os.environ.get("EMBEDDING_API_KEY", ""),
        base_url=base_url,
    )


async def init() -> None:
    """Called once at worker startup. No-op if EMBEDDING_BASE_URL isn't set
    (checked first, before spending time on $PATH scan + --help calls —
    shell-hub degrades to search() always returning [], same
    graceful-absence shape as agent_brain/mcp_hub when their own config is
    unset) or if discovery finds nothing to index."""
    global _embedder, _collection
    _embedder = _build_embedder()
    if _embedder is None:
        logger.info("shell_hub: EMBEDDING_BASE_URL not set, search() will return no results")
        return

    catalog = await _build_catalog()
    if not catalog:
        logger.info("shell_hub: no discoverable commands beyond common excludes, skipping index build")
        return

    # Rebuilt fresh every startup — tenant-worker pods are ephemeral, and
    # the discovered catalog is small, so there's no reason to persist the
    # index across restarts.
    shutil.rmtree(_COLLECTION_PATH, ignore_errors=True)
    schema = zvec.CollectionSchema(
        name="shell_hub",
        fields=[
            zvec.FieldSchema(name="name", data_type=zvec.DataType.STRING),
            zvec.FieldSchema(
                name="description", data_type=zvec.DataType.STRING, index_param=zvec.FtsIndexParam()
            ),
        ],
        vectors=zvec.VectorSchema(
            name="embedding", data_type=zvec.DataType.VECTOR_FP32, dimension=_embedder.dimension
        ),
    )
    collection = await asyncio.to_thread(zvec.create_and_open, path=_COLLECTION_PATH, schema=schema)

    vectors = await asyncio.gather(*(asyncio.to_thread(_embedder.embed, e["description"]) for e in catalog))
    docs = [
        # Hash-derived id, not the raw command name — found necessary by
        # testing, not assumed: zvec rejects doc ids containing certain
        # characters, and a real $PATH scan can turn up names with
        # anything in them (e.g. a unicode-obfuscated binary). name/
        # description live in fields regardless, so nothing is lost.
        zvec.Doc(id=hashlib.sha256(entry["name"].encode()).hexdigest()[:16], vectors={"embedding": vector}, fields=entry)
        for entry, vector in zip(catalog, vectors)
    ]
    await asyncio.to_thread(collection.insert, docs)
    _collection = collection
    logger.info("shell_hub: discovered and indexed %d command(s)", len(catalog))


async def search(query: str, top_k: int = 5) -> list[dict]:
    """Shaped like mcp_hub.call_tool("search_tools", ...)'s own results
    ({server, tool, description, input_schema}) so tools.search_tools can
    merge both into one list without the model needing to know which
    source a given candidate came from — server is always "shell",
    tool is always "shell_exec" (invocation always goes through shell_exec
    directly; shell-hub entries describe a capability, they don't add a new
    invocation path — see module docstring)."""
    if _collection is None or _embedder is None:
        return []

    query_vector = await asyncio.to_thread(_embedder.embed, query)
    results = await asyncio.to_thread(
        _collection.query,
        [
            zvec.Query(field_name="embedding", vector=query_vector),
            zvec.Query(field_name="description", fts=zvec.Fts(query_string=query)),
        ],
        topk=top_k,
        reranker=zvec.RrfReRanker(rank_constant=60),  # matches agent-brain's own RRF rrfK=60
        output_fields=["name", "description"],
    )
    return [
        {
            "server": "shell",
            "tool": "shell_exec",
            "description": doc.fields["description"],
            "input_schema": _SHELL_EXEC_INPUT_SCHEMA,
        }
        for doc in results
    ]
