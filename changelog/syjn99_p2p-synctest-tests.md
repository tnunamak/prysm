### Fixed

- Sync service background goroutines (`processDataColumnLogs` & the quic-v1 stream-reset delay) now respect service shutdown.
- Remove `prunePendingGloasColumns` function as it races with genesis clock at startup. Instead include `pruneStaleGloasColumns` in `p2pHandlerControlLoop`.
- Committee cache (process-wide variable) fill tracking no longer uses a `sync.WaitGroup` that crashes when mixed with `testing/synctest` bubbles.
- Unstoppable `go-cache` janitor goroutines are disabled, with expired entries reclaimed lazily.

### Ignored

- Migrated `beacon-chain/p2p` and `beacon-chain/sync` p2p tests to `testing/synctest` bubbles with hosts on an in-memory simulated network (`x/simlibp2p`), following libp2p/go-libp2p-pubsub#686. Make tests much faster and deterministic.