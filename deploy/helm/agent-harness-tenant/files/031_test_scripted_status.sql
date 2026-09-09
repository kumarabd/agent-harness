-- Scenario fixtures can now drive the model-authored output fields
-- (docs/components/turn-pipeline.md's `status` + `next_step`) that a real
-- report_status call would carry — so a scripted scenario can exercise the
-- status="blocked" terminal branch, the no-progress guard, and the
-- iteration-ceiling raise, none of which the fixture path could reach before.
--
-- Both nullable: absent ⇒ model_call.py synthesizes status from tool-call
-- presence exactly as before, and next_step stays None. Test-only table.
ALTER TABLE _test_scripted_responses ADD COLUMN status    text  DEFAULT NULL;
ALTER TABLE _test_scripted_responses ADD COLUMN next_step  jsonb DEFAULT NULL;
