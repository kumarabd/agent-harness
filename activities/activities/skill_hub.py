"""skill-hub — docs/05-architecture-domain-control-loops.md, docs/components/
turn-pipeline.md ("Skills"). The local, in-process discovery index for
authored domain-workflow "skills" — the same in-process zvec hybrid
vector+FTS mechanism shell_hub.py already uses for local tool discovery,
repurposed here for a catalog sourced from skills.SKILLS instead of a $PATH
scan.

Deliberately NOT mcp-hub-mediated (docs/05-architecture-domain-control-loops.md,
"Discovery and Invocation": "Not mcp-hub-mediated, not shared across tenants,
not manually curated [there]"). A separate collection/embedder/path from
shell_hub's own module-level state, so a rebuild or failure of one is
independent of the other — they just happen to share the same underlying
mechanism, not the same running instance.

No --help subprocess calls: unlike a discovered shell command, a skill's
description is already authored text on its registry entry (skills.SKILLS),
not something to extract from a binary. Built once at tenant-worker startup
(init(), called from tenant_worker.py alongside shell_hub.init()).
"""

from __future__ import annotations

import asyncio
import hashlib
import logging
import os
import re
import shutil

import zvec

from . import skills

logger = logging.getLogger(__name__)

_COLLECTION_PATH = "/tmp/agent-harness-skill-hub"

_embedder: "zvec.OpenAIDenseEmbedding | None" = None
_collection: "zvec.Collection | None" = None

# name -> full registry entry ({name, description, input_schema, workflow_type}),
# so search() can return the whole entry without round-tripping input_schema/
# workflow_type through zvec's own doc fields (only name/description need to be
# indexed — the rest is a plain local dict lookup by name).
_BY_NAME: dict[str, dict] = {}


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
    (same graceful-absence shape as shell_hub/agent_brain/mcp_hub when their
    own config is unset) or if skills.SKILLS is empty."""
    global _embedder, _collection
    _embedder = _build_embedder()
    if _embedder is None:
        logger.info("skill_hub: EMBEDDING_BASE_URL not set, search() will return no results")
        return

    catalog = skills.SKILLS
    if not catalog:
        logger.info("skill_hub: no registered skills, skipping index build")
        return

    _BY_NAME.clear()
    _BY_NAME.update({entry["name"]: entry for entry in catalog})

    # Rebuilt fresh every startup, same reasoning as shell_hub: tenant-worker
    # pods are ephemeral and the catalog is small.
    shutil.rmtree(_COLLECTION_PATH, ignore_errors=True)
    schema = zvec.CollectionSchema(
        name="skill_hub",
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
        # Hash-derived id — same rationale as shell_hub: keeps doc ids clear
        # of whatever character set a skill's own name happens to use.
        zvec.Doc(
            id=hashlib.sha256(entry["name"].encode()).hexdigest()[:16],
            vectors={"embedding": vector},
            fields={"name": entry["name"], "description": entry["description"]},
        )
        for entry, vector in zip(catalog, vectors)
    ]
    await asyncio.to_thread(collection.insert, docs)
    _collection = collection
    logger.info("skill_hub: indexed %d skill(s)", len(catalog))


# Same FTS-safety reasoning as shell_hub._fts_safe — zvec's tantivy-style FTS
# grammar treats a handful of characters as operators, and a natural-language
# query is never trustworthy FTS input as-is.
_FTS_MAX_TOKENS = 32


def _fts_safe(query: str) -> str:
    tokens = re.findall(r"[a-z0-9]+", (query or "").lower())
    return " ".join(tokens[:_FTS_MAX_TOKENS])


async def search(query: str, top_k: int = 5) -> list[dict]:
    """Returns matched skill registry entries: {name, description,
    input_schema, workflow_type}. No {server, tool} shape here — a skill
    isn't a resolved mcp-hub/shell-hub tool identity, it's dispatched as its
    own child workflow by turn.go, keyed by workflow_type directly."""
    if _collection is None or _embedder is None:
        return []

    query_vector = await asyncio.to_thread(_embedder.embed, query)
    queries = [zvec.Query(field_name="embedding", vector=query_vector)]
    fts_query = _fts_safe(query)
    if fts_query:
        queries.append(zvec.Query(field_name="description", fts=zvec.Fts(query_string=fts_query)))
    results = await asyncio.to_thread(
        _collection.query,
        queries,
        topk=top_k,
        reranker=zvec.RrfReRanker(rank_constant=60),  # matches shell_hub/agent-brain's own RRF rrfK=60
        output_fields=["name", "description"],
    )
    matched = []
    for doc in results:
        entry = _BY_NAME.get(doc.fields["name"])
        if entry is None:
            continue  # stale index vs. registry, shouldn't happen post-init — skip rather than fabricate
        matched.append(
            {
                "name": entry["name"],
                "description": entry["description"],
                "input_schema": entry["input_schema"],
                "workflow_type": entry["workflow_type"],
            }
        )
    return matched
