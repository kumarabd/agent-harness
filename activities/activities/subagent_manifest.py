"""SubagentManifest activity — closes docs/components/session-filesystem.md's
"Resolved: Subagent Merge-Back Mechanics" gap by producing the changed-file
list a completed subagent surfaces to its parent's next ModelCall.

Fires exactly once, after a subagent child workflow completes (turn.go's
subagent branch), against the subagent's own turn_id (which IS its
tool_call_id per docs/components/temporal-workflow.md, "Resolved: Reference/ID
Schema"). Writes the manifest into `tool_calls.result` alongside whatever
status the subagent already recorded — same column lcm.py already reads
verbatim when reconstructing tool results into the parent's context, so
nothing else has to change to make the parent see this.

The design doc originally said the manifest would be "a query over
session_filesystem_leases for every row whose path falls under the
subagent's subtree." That doesn't work as-implemented: leases today are
directory-level (one row per session/subagent working dir, not per file)
and are DELETED on release (leases.py). So this activity uses the shared
PV filesystem as the source of truth instead — os.walk of the subagent's
subtree — matching the doc's spirit ("no new base-hash or snapshot
mechanism") since the PV is already the durable file store; the leases
table's job is coordinating live writers, not historical accounting.

Files are listed with sizes and mtimes (both cheap to stat, both useful
to the model — mtime distinguishes files the subagent actually created/
modified from files that just happened to live under its subtree if it
was ever seeded with anything). Path is relative to the subagent's own
subtree root, not the PV root — so the model sees `foo.txt` rather than
`/session/{key}/sub/1/foo.txt`, and the eventual merge_subagent_output
call takes those same relative paths.

No new schema. No changes to leases. If the subagent's subtree doesn't
exist yet (a subagent that never wrote a file, or was cancelled before
any tool ran), the manifest is empty — this activity just records that
fact honestly rather than skipping the write, so the parent's ModelCall
sees a real "no files changed" result rather than the ambiguous no-write.
"""

from __future__ import annotations

import json
import logging
import os

from temporalio import activity

from . import claim_check, ids
from .tools import resolve_session_dir

logger = logging.getLogger(__name__)


class SubagentManifestActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="SubagentManifest")
    async def __call__(self, subagent_turn_id: str) -> None:
        fs_path = ids.session_fs_path(subagent_turn_id)
        subtree_root = resolve_session_dir(fs_path)

        files: list[dict] = []
        if os.path.isdir(subtree_root):
            for dirpath, dirnames, filenames in os.walk(subtree_root):
                # Prune .claim-check/ so tool-output artifacts (large
                # stdout/stderr routed through the PV by claim_check.py)
                # don't surface as subagent-authored changed files in the
                # manifest — they're plumbing, not merge candidates.
                # Standard os.walk pruning idiom (mutate dirnames in-place).
                dirnames[:] = [d for d in dirnames if not claim_check.is_claim_check_dir(d)]
                for name in filenames:
                    absolute = os.path.join(dirpath, name)
                    # Skip anything that isn't a regular file (dangling
                    # symlinks, sockets, devices) — the merge tool can't
                    # meaningfully copy those and the model has no use for
                    # them in a changed-file manifest.
                    try:
                        st = os.stat(absolute)
                    except OSError:
                        continue
                    if not os.path.isfile(absolute):
                        continue
                    relative = os.path.relpath(absolute, subtree_root)
                    files.append(
                        {
                            "path": relative,
                            "size_bytes": st.st_size,
                            "mtime": st.st_mtime,
                        }
                    )

        # Sorted for a deterministic manifest — os.walk order isn't
        # guaranteed across platforms/filesystems, and a stable listing is
        # kinder to the model (and to test diffs) than a shuffled one.
        files.sort(key=lambda f: f["path"])

        manifest = {
            "subagent_turn_id": subagent_turn_id,
            "changed_files": files,
        }

        # The subagent's own tool_calls row (tool_call_id == subagent_turn_id)
        # was already UPDATEd by its own workflow's exit path with
        # status=ok/cancelled/error and no result. Merge the manifest into
        # whatever result is already there rather than clobbering — a
        # cancelled subagent might have a real reason/side_effect set that
        # we shouldn't drop.
        row = await self._pool.fetchrow(
            "SELECT result FROM tool_calls WHERE tool_call_id = $1",
            subagent_turn_id,
        )
        if row is None:
            # A subagent's tool_calls row is written by the parent's ModelCall
            # (model_call.py); if it isn't there, something is wrong upstream
            # — propagate rather than silently skip.
            raise RuntimeError(
                f"SubagentManifest: no tool_calls row for {subagent_turn_id!r}"
            )
        existing = json.loads(row["result"]) if row["result"] else {}
        if isinstance(existing, dict):
            existing["manifest"] = manifest
            merged_result = existing
        else:
            # Existing result isn't a dict (unexpected shape) — nest it under a
            # neutral key rather than losing it or crashing.
            merged_result = {"previous": existing, "manifest": manifest}

        await self._pool.execute(
            "UPDATE tool_calls SET result = $2 WHERE tool_call_id = $1",
            subagent_turn_id,
            json.dumps(merged_result),
        )
        logger.info(
            "SubagentManifest[%s]: %d file(s)",
            subagent_turn_id,
            len(files),
        )
