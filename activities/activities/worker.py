"""Activity worker entrypoint. Registers ModelCall, ToolCall, InsertMessage,
Persist, Deliver, and CompressContext on the configured task queue and polls a
Temporal server for activity tasks. Run alongside the Go workflow worker
(workflows/cmd/worker), which registers the Session Coordinator and Turn
Workflow on the same task queue and dispatches activities to this process by
name.

Configured via env vars (not hardcoded) so this process is deployable — see
deploy/docker/activity-worker.Dockerfile and deploy/helm/agent-harness:

    TEMPORAL_ADDRESS     Temporal frontend host:port. Default: localhost:7233.
    TEMPORAL_NAMESPACE   Temporal namespace. Default: default.
    TEMPORAL_TASK_QUEUE  Task queue name. Default: agent-loop. Must match the
                         Go workflow worker's TEMPORAL_TASK_QUEUE.
    POSTGRES_HOST/PORT/DB/USER/PASSWORD  This tenant's Postgres instance
                         (docs/components/multi-tenancy.md). See db.py.

Usage:
    python -m activities.worker
"""

from __future__ import annotations

import asyncio
import logging
import os

from temporalio.client import Client
from temporalio.worker import Worker

from .compress_context import compress_context
from .db import create_pool
from .deliver import DeliverActivity
from .insert_message import InsertMessageActivity
from .model_call import ModelCallActivity
from .persist import PersistActivity
from .tool_call import ToolCallActivity


async def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")

    address = os.environ.get("TEMPORAL_ADDRESS", "localhost:7233")
    namespace = os.environ.get("TEMPORAL_NAMESPACE", "default")
    task_queue = os.environ.get("TEMPORAL_TASK_QUEUE", "agent-loop")

    pool = await create_pool()

    client = await Client.connect(address, namespace=namespace)
    # Worker needs the bound __call__ methods, not the instances themselves —
    # @activity.defn attaches its metadata to the decorated function, and
    # `instance.__call__` (bound) carries that metadata; the bare instance
    # does not.
    worker = Worker(
        client,
        task_queue=task_queue,
        activities=[
            ModelCallActivity(pool).__call__,
            ToolCallActivity(pool).__call__,
            InsertMessageActivity(pool).__call__,
            PersistActivity(pool).__call__,
            DeliverActivity(pool).__call__,
            compress_context,
        ],
    )
    logging.getLogger(__name__).info(
        "activity worker starting: temporal=%r namespace=%r task_queue=%r", address, namespace, task_queue
    )
    try:
        await worker.run()
    finally:
        await pool.close()


if __name__ == "__main__":
    asyncio.run(main())
