// Package integration holds black-box tests that build and run the real
// cmd/server (and cmd/miauthctl) binaries as subprocesses and drive them
// over real HTTP/OS-signal boundaries, the way an operator or Aria
// itself would. This is deliberately a different layer from:
//
//   - internal/httpserver's Run tests, which exercise the HTTP
//     lifecycle in-process but never build cmd/server's own
//     configuration/migration/job-manager wiring;
//   - contract/aria_client, which decodes a pre-started bin/server's
//     responses with Aria's own pinned parser but does not itself start,
//     restart, or signal the server process.
//
// Issue #13 (release gate) AC1 specifically requires evidence that a
// clean-environment deploy of the actual binary migrates, becomes ready,
// and shuts down gracefully on SIGTERM; AC4 requires evidence that a
// post succeeds while the LLM provider is down and that recovery
// reprocesses the pending job without manual intervention. Both are
// most directly proven by actually running the binary, not by unit
// testing the functions it wires together.
package integration
