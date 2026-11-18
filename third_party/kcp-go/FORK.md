# Magic's kcp-go fork

Based on [`github.com/xtaci/kcp-go/v5` v5.7.5](https://github.com/xtaci/kcp-go/tree/v5.7.5), copied from the Go module cache. The module path and MIT license are unchanged; Magic selects the fork with a local `replace` in its `go.mod`. The upstream `examples/`, `wireshark/`, and `assets/` directories are omitted.

## Changes

- **OOB without FEC.** An unencrypted no-FEC OOB datagram is `| conv (u32 LE) | 0x01f3 (u16 LE) | payload |`. The low marker byte `0xf3` is not a raw KCP command (81–84), and the full marker is distinct from the existing FEC OOB type `0x00f3`, even when the conversation ID is `0xffffffff`. Both the listener and the session recognize the six-byte header, including with an empty payload. The no-FEC marker is accepted only by sessions without a FEC decoder. The existing FEC-mode layout is unchanged. OOB packets still go through the same outbound queue and configured encryption as reliable traffic, but are never acknowledged, retransmitted, or FEC-protected. The receive callback runs on the input path and must not block. This lets Magic send latest-wins mouse positions without waiting for a lost older position; keys, buttons, wheel events, and handoff can remain ordered on KCP. Both endpoints need this fork to use the no-FEC OOB format; the KCP stream format is unchanged.
- **Per-session `SetMinRTO(ms)`.** The adaptive estimator clamps its RTO to the configured floor and the existing 60-second ceiling. The override survives `SetNoDelay` in either order; zero restores the mode's default floor. Magic can tune its latency-sensitive control session to a 15 ms floor instead of the stock 30 ms no-delay floor without changing the bulk session or overriding larger measured RTOs.
- **`SetDupPush(n)`.** The flush path records whether each datagram contains at least one PUSH, including datagrams that also carry ACKs. After framing and encryption, the output pipeline sends `n` extra copies of each marked datagram, including retransmissions. It does not add copies of pure ACK or window-control traffic, FEC parity, or OOB. The default is zero; the deprecated `SetDUP` still provides blanket duplication when explicitly enabled. This lets the KVM control session trade a small amount of bandwidth for faster delivery of keys and buttons despite occasional Wi-Fi loss, without doubling ACK traffic or copying the bulk session's stream.

## Tests

Run the fork's race-enabled test suite from the Magic root:

```sh
(cd third_party/kcp-go && go test -race ./...)
```

The fork regressions cover bidirectional OOB with empty, one-byte, and maximum-size payloads alongside a reliable stream (plain, CFB, and AEAD); actual UDP copy counts and mixed ACK/PUSH packet packing; and adaptive RTO with millisecond-scale loopback RTT samples in both setter orders. The upstream FEC OOB tests remain in the suite.

The upstream 1 GiB and 6 GiB echo stress tests are opt-in (`KCP_STRESS_TEST=1`) so they don't dominate the default race run.
