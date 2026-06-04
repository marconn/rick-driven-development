# 0005 — Stall/dwell alert

- **Track:** Telemetry · **SP:** 2
- **Status:** Blocked · **Depends on:** 0003 · **Blocks:** —
- **Spec:** [Section 6 — Observability](../persona-extensibility-and-dispatch-redesign.md#6-observability)

## Context

Silent strands are the top operational pain: a workflow blocks on an unsatisfied
readiness condition and no one is paged. Once the dwell-time projection (0003)
exists, wire an alert on any correlation whose persona sits in a blocked state
past a threshold **derived from the measured baseline**, not guessed.

## Scope

- **In:** an alert on blocked-state dwell exceeding a baseline-derived threshold;
  document the query.
- **Out:** the projection itself (0003); the projection-updater liveness alert
  (that ships with 0010).

## Files

- Ops/alerting config (repo's alerting location).
- Document the detection query in the repo, in the style of the existing
  synchronous-feedback observability reference.

## Implementation notes

- Threshold = a high percentile of the observed `dispatch_dwell_seconds` baseline
  (record the chosen value and its basis). Do **not** hardcode a round number.
- Bucket by persona and stall reason so the alert names the suspect.

## Acceptance criteria

- [ ] An induced stall (block a readiness condition in a test/staging workflow)
      fires the alert within the threshold window.
- [ ] The threshold value and its statistical basis are documented.
- [ ] No false page on normal parallel fan-out (expected transient blocks).

## Tests

- Validate the query against a known-stalled correlation in a fixture DB.

## Rollback

Alerting config only; disable the rule.
