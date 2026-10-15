# MAS platform rollout readiness demo (Go)

A read-only, deterministic **multi-region Kubernetes rollout readiness review**
using [emmett08/mas](https://github.com/emmett08/mas). A release coordinator delegates
to regional leads, which consult capacity, reliability, and security specialists.
Specialists cross-check one another before a regional result can pass the release
gate. An unsigned image, insufficient surge capacity, or unavailable telemetry
cannot be voted away by other successful checks.

The application is Go, with no third-party Go dependencies. MAS is a **Rust
runtime**, not a Go library: this demo drives its real administrative CLI and
SQLite mailboxes. It does not reimplement the MAS protocol or use a fake bus.
No Kubernetes cluster, API keys, Python, LLMs, or paid services are needed.

## Run

Requirements: Go 1.24+, Rust 1.99.0, Cargo, a C compiler (for bundled SQLite),
and Linux for the verified setup below. The demo was tested against MAS commit
[`9d9845990843f5e3464395da7bd60bb50ffb2dda`](https://github.com/emmett08/mas/commit/9d9845990843f5e3464395da7bd60bb50ffb2dda).
Other runtime revisions may change the CLI contract.

Use absolute paths for both repositories. Set `DEMO_DIR` to your checkout; the
following downloads the pinned upstream source into a fresh temporary directory:

```sh
export DEMO_DIR=/absolute/path/to/mas-demo
export MAS_SOURCE="$(mktemp -d /tmp/mas-source.XXXXXX)"
export MAS_REV=9d9845990843f5e3464395da7bd60bb50ffb2dda
curl --fail --location "https://codeload.github.com/emmett08/mas/tar.gz/$MAS_REV" \
  --output "$MAS_SOURCE/source.tar.gz"
tar -xzf "$MAS_SOURCE/source.tar.gz" -C "$MAS_SOURCE" --strip-components=1
cargo build --release --locked \
  --manifest-path "$MAS_SOURCE/rust-protocol/Cargo.toml" --bin mas
export MAS_BIN="$MAS_SOURCE/rust-protocol/target/release/mas"

go -C "$DEMO_DIR" run . -mas-bin "$MAS_BIN" -scenario healthy
```

Alternatively, supply an already-built compatible `mas` executable. When
`-mas-bin` is omitted, the executable is resolved from `PATH`.

JSON goes to stdout; the retained evidence directory is printed to stderr.
The healthy report contains `"decision": "proceed"`, two `ready` regions, six
checks with evidence, and MAS usage of **28 messages and zero external calls**.
Database paths vary, but the readiness decisions and check evidence are repeatable.

### Scenarios

```sh
go -C "$DEMO_DIR" run . -mas-bin "$MAS_BIN" -scenario capacity
go -C "$DEMO_DIR" run . -mas-bin "$MAS_BIN" -scenario unsafe-image
go -C "$DEMO_DIR" run . -mas-bin "$MAS_BIN" -scenario missing-telemetry
```

| Scenario | Changed evidence | Decision |
| --- | --- | --- |
| `healthy` | Both regions have sufficient capacity, error budget, and safe images | `proceed` |
| `capacity` | West has only 3 free pod slots; the rollout needs 4 | `hold` |
| `unsafe-image` | East's image is unsigned; security and reliability reject it | `hold` |
| `missing-telemetry` | West cannot establish its error budget or safe surge allowance | `inconclusive` |

The fixture scales from 6 to 8 replicas with 2 surge replicas, so the capacity
check needs `8 - 6 + 2 = 4` free pod slots. At least 20% error budget permits a
surge of 2; below that threshold the permitted surge is 0. Security requires a
signed, nonprivileged workload whose surge does not exceed its policy limit.
These are illustrative policies, not universal Kubernetes sizing or SLO rules.

`hold` takes precedence over `inconclusive` when both blockers and missing evidence
exist. Only all-ready checks in all regions permit `proceed`.
**All successfully evaluated scenarios exit 0**, including holds. Inspect the JSON
decision, not the process exit status, for readiness. Invalid arguments, runtime
errors, expired/incomplete tasks, and timeouts exit nonzero without a decision.

## Why combine topologies?

```text
                         release
                         /     \
                   east-lead   west-lead       delegation tree
                     / | \      / | \
                    C  R  S    C  R  S          regional stars
                    \--mesh/   \--mesh/         peer review

              C = capacity, R = reliability, S = security
```

- **Tree:** `release ↔ region-lead ↔ specialists` scopes responsibility. The
  coordinator cannot address specialists directly; regions cannot address one
  another.
- **Star:** each regional lead queues three independent readiness requests before
  collecting results. The lead assembles a regional report rather than assuming
  that one successful specialist implies overall readiness.
- **Mesh:** each regional trio has all pairwise routes. Capacity asks reliability
  for a surge allowance; reliability asks security for image attestation; security
  asks capacity for the proposed surge size. Each pair exchanges real MAS requests
  and correlated results; peer evidence affects the checks.

These patterns overlap in **one explicit directed permission graph**. Every
request route also has a reply route. Topology controls who may communicate; it
does not schedule execution. This Go host deliberately executes handlers
serially, even though star requests are queued together. It is not a concurrency
benchmark.

Each region uses seven tasks: one delegation, three specialist checks, and three
peer checks. Across two regions that is **14 tasks / 28 messages**, bounded by
100 messages, 256 KiB of cumulative wire data, mailbox capacity 8, and a 120-second
MAS deadline. Pure fixture computations do not reserve external calls.

For each exchange, Go asks MAS to encode the envelope, sends it, receives and
validates the exact delivery, checks request actionability, acknowledges receipt,
then executes the handler using the delivered input. Results travel back through
MAS before aggregation. The final report also requires every task to be completed
and the run not to be expired. A protocol task being `completed` means its
assessment was returned, **not** that the assessment passed.

## Inspect retained evidence

By default, each invocation creates a private temporary directory and leaves it
available for inspection. For a predictable location, supply a **new** directory:

```sh
go -C "$DEMO_DIR" run . -mas-bin "$MAS_BIN" \
  -scenario capacity -state-dir "$DEMO_DIR/.mas-state"
"$MAS_BIN" status "$DEMO_DIR/.mas-state/state.sqlite" rollout
```

The directory contains `topology.json` and the real `state.sqlite` database.
Status shows the frozen routes, task outcomes, usage, and delivery/acknowledgement
records. The local `.mas-state` directory and default Go binary are gitignored.
Existing directories are refused to avoid overwriting or accidentally reusing a
previous run. Move or remove evidence explicitly before reusing a directory.
Temporary evidence is not automatically cleaned up.

The Go workflow has a total timeout of 30 seconds by default; `-timeout 60s`
adjusts it, up to two minutes. On failure, inspect the printed state directory:
a timed-out subprocess may already have committed a transaction. The demo does
not automatically retry or resume an interrupted workflow. MAS mailbox
redelivery and idempotent sends do not themselves supply workflow recovery.

## Validate

```sh
go -C "$DEMO_DIR" test ./...
go -C "$DEMO_DIR" vet ./...
go -C "$DEMO_DIR" build -o /tmp/mas-rollout-demo .

# Include real-runtime integration tests (otherwise these explicitly skip):
MAS_BIN="$MAS_BIN" go -C "$DEMO_DIR" test -race -count=1 ./...
```

Unit tests cover policy boundaries, fail-closed aggregation, and graph boundaries.
Integration tests run every scenario against real MAS, verify 14 completed tasks
and empty mailboxes, test forbidden routes, idempotent sends, restart redelivery,
envelope matching, and cancelled-request rejection.

## Safety and scope

All agents are deterministic Go functions under **one trusted host identity**,
using synthetic observations. No manifests are applied, no cluster is queried,
and no image signature is actually verified. This demonstrates orchestration and
evidence flow, not production deployment approval or AI reasoning.

The administrative CLI accepts the host's asserted actor; topology rejection is
not agent authentication or isolation. Do not expose this CLI, database, or host
to untrusted agents. A production extension needs MAS's capability gateway and
appropriate process isolation, independently obtained telemetry and attestations,
operation-specific validation, durable scheduling/recovery, and explicit human
approval plus idempotency/reconciliation for any external effects. Receipt,
task completion, and peer agreement do not establish truth or exactly-once
deployment.