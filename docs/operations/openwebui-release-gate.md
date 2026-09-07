# Open WebUI release gate (OWUI-R) evidence

Issue #54 (OWUI-R, `docs/roadmap/openwebui.md` §"OWUI-R: optional release
gate") lists eight acceptance criteria for treating the Open WebUI outbound
bridge as release-ready. This document is that traceability table — each AC
bullet mapped to the fixture-based test or documented inspection that is its
evidence — the same role `docs/operations/security-regression.md` plays for
Issue #13's AC8.

**Out of scope for this document, by owner decision:** target-instance
evidence (persistent chat creation/continuation, default-model permission,
and completion finish against a real Open WebUI instance) and the
operational finalization of credential rotation ownership both require
details Issue #50 (Open WebUI umbrella) still tracks as TBD — a dedicated
service account and rotation owner, the pinned production instance version,
the allowlisted origin, size/timeout/rate limits, the default-model access
grant procedure, and the socket.io streaming decision. Six of the eight AC
bullets below have fixture/inspection evidence as of this PR; the remaining
two are deferred to a follow-up once those TBDs resolve. **Issue #54 is not
closed by this PR series** — see the status note at the end of this
document and the matching note in `docs/roadmap/openwebui.md`.

## Acceptance criteria → evidence

| # | Acceptance criterion (roadmap wording) | Evidence |
| --- | --- | --- |
| 1 | Feature-off regression proves the existing #1 auth/post/reply/thread/source-ingestion behavior is unchanged. | `internal/httpserver/openwebui_enqueue_test.go`: `TestNotesCreate_OpenWebUIBridgeNilNeverEnqueues` |
| 2 | Owner root → assistant → follow-up → assistant is restored after restart, with the same local thread and local `reply_to_id` tree. | `internal/httpserver/openwebui_restart_test.go`: `TestOpenWebUIRestart_ThreadAndLinkStatePersistAcrossIndependentHarnessInstances` (Issue #54 OWUI-R PR1) |
| 3 | A reply to an earlier local ancestor remains a sibling in the same local thread and uses a distinct remote chat; no remote branch is silently mixed into the linear chat. | `internal/httpserver/openwebui_branch_isolation_test.go`: `TestOpenWebUIBranchIsolation_ReplyToEarlierAncestorStartsNewRemoteChatButStaysInSameLocalThread` (Issue #54 OWUI-R PR2), backed by the unit-level `internal/openwebui/path_test.go`: `TestSelectBranch_NoContinuationForReplyToEarlierNode`, `TestSelectBranch_NoContinuationForSecondReplyToSameHead` and `internal/openwebui/turnjob_test.go`: `TestTurnJob_StaleBranchIsolation_CompletionOnlyMovesItsOwnLink` |
| 4 | Provider outage still saves the Aria post; duplicate delivery creates no second assistant entry or remote chat. | `internal/httpserver/openwebui_outage_test.go`: `TestOpenWebUIOutage_NotesCreateSucceedsRegardlessOfProviderReachability`, `TestOpenWebUIOutage_DuplicateJobDeliveryNeverDuplicatesReplyOrRemoteChat` (Issue #54 OWUI-R PR2), backed by the unit-level `internal/openwebui/bridge_test.go`: `TestEnqueueTurn_DuplicateDeliveryIsIdempotent` and `internal/openwebui/turnjob_test.go`: `TestTurnJob_DuplicateDelivery_NeverCallsProvider`, `TestTurnJob_ContinueRetry_LookupErrorResendsSameIDs`, `TestTurnJob_StartChat_TurnPhaseFailureAfterCreation_RetriesRatherThanFreezing` |
| 5 | Initial remote chat creation response loss becomes `ambiguous` (and may become `dead` only through explicit recovery) and never silently creates a duplicate chat; no new creation, continuation, or automatic retry occurs until owner/operator recovery. | `internal/httpserver/openwebui_ambiguity_test.go`: `TestOpenWebUIAmbiguity_ChatCreationResponseLossNeverAutoRetriesOrDuplicatesChat`, `TestOpenWebUIAmbiguity_OwnerConfirmLinkIsTheOnlyWayToRecoverAReply` (Issue #54 OWUI-R PR2), backed by the unit-level `internal/openwebui/turnjob_test.go`: `TestTurnJob_StartChat_TimeoutFreezesLinkAmbiguousAndNeverRecreates`, `TestTurnJob_LinkStateGuard_AmbiguousFailedDeadNeverCallProvider` and `internal/openwebui/recovery_test.go`: `TestConfirmLink_*`, `TestAbandonLink_*`, `TestFreezeLink_*` |
| 6 | Target-instance evidence covers persistent chat creation/continuation, default-model permission, completion finish, and any enabled stream finish. | **Deferred.** Requires a real Open WebUI instance and a provisioned service account, both TBD in Issue #50. |
| 7 | Raw provider credentials, session capabilities, cookies, prompts, and stream chunks are absent from logs, traces, fixtures, and error responses; encrypted, access-controlled backups may contain local post/assistant bodies and opaque remote IDs, but never raw provider secrets. | `docs/operations/security-regression.md`'s "Log redaction" table: the existing `internal/openwebui/provider_test.go`/`internal/provider/openwebui/client_test.go`/`cmd/openwebuictl/main_test.go` rows cover logs/traces/fixtures/error responses; the backup row added by Issue #54 OWUI-R PR3 covers backups, citing `internal/openwebui.RegistryConfig` (`internal/openwebui/registry.go`) holding only `SecretRef` (a configuration key name, never the key itself) and `docs/decisions/0005-openwebui-boundary.md` D10 as the decision record for that storage boundary |
| 8 | Same-remote-chat branch management, regeneration, provider edit/delete, existing-chat import/list/pull, and history-browsing behavior is not advertised as successful API/UI capability. | **Deferred.** Not yet independently verified and documented; tracked alongside the runbook and target-instance work once Issue #50's TBDs resolve. |

## Status (as of Issue #54 OWUI-R PR3)

6 of 8 acceptance criteria have fixture-based or documented-inspection
evidence. The remaining 2 (target-instance evidence and the same-remote-chat
capability-advertising restriction, rows 6 and 8 above) are pending Issue
#50's TBD resolution and are not addressed by this PR series. Issue #54
stays open until they are.
