# Work log

The assignment asks for approximate time spent. This file records measured elapsed work sessions; future review or modification sessions should be appended before submission.

| Date | Session | Activity | Elapsed time |
|---|---|---|---:|
| 2026-09-28 | 17:01–17:22 IST | Requirements review, architecture, implementation, database constraints, concurrency and HTTP contract tests, reviewer documentation, Docker demo, stress passes, and final verification | 21 minutes |
| 2026-09-28 | 17:22–17:54 IST | Independent code review, concurrent-startup reproduction, migration and idempotency-race fixes, regression tests, documentation updates, and race-enabled verification | 32 minutes |
| 2026-09-28 | 17:54–18:05 IST | Critical-endpoint load harness, isolated load runs, invariant stress checks, populated-report measurement, and results documentation | 11 minutes |
| 2026-09-28 | 18:11–18:31 IST | Production-readiness review against the brief; bounded lock waits with retryable 503, 413 for oversized bodies, coupon list in the report, single replay check, case-insensitive coupon codes, cart-to-order link; integration and handler tests, load-harness rerun; Scalar reference at `/docs` checked in a browser against the Compose build; documentation | 20 minutes |

**Recorded total so far: approximately 84 minutes.** This unusually short elapsed time reflects extensive AI-assisted implementation and automated verification. Session start times are bounded by the preceding commit, so each figure is accurate to within a few minutes.
