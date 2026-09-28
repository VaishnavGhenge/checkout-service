# Local Load-Test Baseline

## Scope and environment

This is a development-machine baseline, not a production capacity claim. It was captured on 2026-09-28 with:

- the API and load generator running locally on macOS arm64;
- Go 1.26.4;
- PostgreSQL 17 Alpine in Docker using the isolated `checkout_test` database;
- the normal API connection pool limit of 20; and
- synchronous JSON request logging enabled.

The client measured end-to-end HTTP latency. The run used 64 read workers, 32 checkout-workflow workers, a 10-second empty-database read phase, a 15-second checkout phase, and a 5-second populated-database read phase. The checkout database contained 12,212 orders before the populated read phase.

## Results

| Operation | Requests | Requests/sec | p50 | p95 | p99 | Result |
|---|---:|---:|---:|---:|---:|---|
| `GET /products`, empty database | 82,423 | 8,233.3 | 3.730 ms | 5.090 ms | 5.809 ms | 100% `200` |
| `GET /admin/report`, empty database | 53,197 | 5,311.8 | 5.761 ms | 7.785 ms | 8.861 ms | 100% `200` |
| `POST /carts` | 12,212 | 813.0 | 2.358 ms | 5.334 ms | 6.607 ms | 100% `201` |
| `POST /carts/{id}/items` | 12,212 | 813.0 | 6.206 ms | 12.191 ms | 15.891 ms | 100% `201` |
| `POST /carts/{id}/checkout` | 12,212 | 812.9 | 17.588 ms | 61.756 ms | 93.991 ms | 100% `201` |
| Exact checkout retry | 12,212 | 812.9 | 5.744 ms | 8.587 ms | 9.758 ms | 100% `201`; same order ID and replay header verified |
| `GET /products`, 12,212 orders | 21,137 | 4,218.1 | 7.538 ms | 9.541 ms | 10.699 ms | 100% `200` |
| `GET /admin/report`, 12,212 orders | 11,201 | 2,234.9 | 14.059 ms | 18.986 ms | 22.441 ms | 100% `200` |

The four-step checkout workflow sustained approximately 813 completed workflows per second, or roughly 3,252 HTTP requests per second across cart creation, item addition, checkout, and exact retry.

## Contention and correctness checks

The same run also applied synchronized bursts to the invariants most likely to fail under overlap:

- 200 simultaneous checkouts competed for 100 limited-stock units: exactly 100 returned `201`, 100 returned `409`, and remaining inventory was zero. Contended checkout p95 was 107.463 ms and p99 was 108.995 ms.
- 20 simultaneous coupon-generation requests targeted one eligible milestone: exactly one returned `201` and 19 returned `409`.
- Two simultaneous carts redeemed the generated coupon: exactly one returned `201` and one returned `409`.
- The final administration report reconciled six successful orders with one generated and redeemed coupon.
- No transport errors, unexpected statuses, duplicate retry orders, oversells, duplicate milestone coupons, or double redemptions were observed.

## Interpretation and limitations

Checkout is the expected bottleneck because it performs the most database work and holds row locks; its p99 was approximately 94 ms at this concurrency. Reporting p99 rose by roughly 2.5 times after 12,212 orders but remained below 23 ms in this short local run.

These figures should not be extrapolated directly to production. The API, generator, and Docker database shared one machine; the run was intentionally short; it did not introduce network latency, connection loss, process restarts, a large product catalog, or millions of historical orders. A production capacity exercise should run the generator on a separate host, define service-level objectives first, capture CPU/I/O/connection-pool/lock-wait metrics, and continue through steady-state and saturation periods.
