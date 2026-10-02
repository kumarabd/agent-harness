-- Progress watchdog (docs/components/turn-pipeline.md, "Progress watchdog").
-- While the model is heads-down for a long stretch, a workflow.Go goroutine
-- inside TurnWorkflow arms a durable timer and, if no user-visible delivery
-- happened before it fired, dispatches a StatusPing activity that writes one
-- transient one-liner here and the gateway pushes it out of band.
--
-- Deliberately its OWN table, not a widened turn_deliveries: that table is the
-- streamed-chunk content ledger (cumulative `content`, a `sent` idempotency
-- guard, consumed by DiscordDeliverChunk). A status ping is a different
-- liveness concept — not transcript, not streamed content, MUST NOT enter LCM
-- context — so it gets its own row shape rather than sharing that ledger.
--
-- `reason`: '' for an ordinary progress ping, 'wedged' for the coordinator's
-- fallback notice when the TurnWorkflow child hit its WorkflowRunTimeout and
-- failTurn could not run.
CREATE TABLE turn_status_pings (
  turn_id     text NOT NULL REFERENCES turns(turn_id),
  seq         int  NOT NULL,
  content     text NOT NULL,
  reason      text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (turn_id, seq)
);
