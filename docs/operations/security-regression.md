# Security regression coverage

Issue #13 (release gate) AC8 requires "security regression tests (request
limits, SSRF, XSS/escaping, prompt injection, log redaction, cookie/token
attributes)". This document is the traceability table from each AC8
bullet (and AC3's related rejection-path bullet, folded into this same
table rather than getting a second one) to the tests that are its
evidence, so a future change that weakens one of these properties has a
named regression suite to break instead of relying on this list being
re-derived from scratch.

This inventory does not add new application code; it records what already
exists and closes the two gaps the Issue #13 plan identified (a symmetric
prompt-injection test for `internal/llmreply`, and explicit
attribute-based XSS payload coverage).

## AC3: unauthorized/expired/replayed/wrong-scope/revoked requests are rejected

Issue #28 (ADR-0002) replaced upstream-Misskey-account authorization with
local, operator-approved MiAuth sessions; see that ADR for how each
upstream AC3 bullet maps onto this design.

The last five rows below are about actors rather than requests. The
first three were added by Issue #52 PR1, which made `actors` able to
hold rows other than the owner and the two reserved presentation actors
(an Open WebUI model's VirtualActor). Since "the owner is the only row
that exists" is no longer what keeps login single-owner, the rule that
presentation actors are wire projections and never additional
login-capable users (AGENTS.md) needs tests of its own. The next two
were added by PR3, which gave the Open WebUI use-case layer its own
owner-only writes and its own caller-supplied-author entry-creation
path — both had to repeat the same "never a presentation actor" check
independently, so both get their own regression coverage. The last row
was added by PR4, whose five owner recovery methods (`ListLinks`,
`DescribeLink`, `ConfirmLink`, `AbandonLink`, `FreezeLink`) and their
`cmd/openwebuictl` CLI are a *third* independent owner-only door — the
same check repeated a third time, and PR4's own reason it must never be
reachable from an HTTP request at all (roadmap: "Manual resolution ...
is an explicit owner/operator action") is exactly why there is no fourth
row for a request-level test here.

| Bullet | Evidence |
| --- | --- |
| Unapproved session never yields a token | `internal/miauth/service_test.go`: `TestApproveAndRejectUnavailableSessions`, `TestRejectAndListPendingSessions` |
| Expired local MiAuth session cannot resume | `internal/miauth/service_test.go`: `TestStartLocalSession_ExpiredCannotResume` |
| Replayed `/api/miauth/{session}/check` consume is rejected | `internal/httpserver/miauth_handlers_test.go`: `TestHandleMiAuthCheck_ApprovalSuccessAndReplay`, `TestHandleMiAuthCheck_ConcurrentCallsHaveExactlyOneWinner` |
| Wrong scope is rejected | `internal/httpserver/scope_middleware_test.go`: `TestRequireScope_RejectsInsufficientScope` |
| Revoked token is rejected | `internal/httpserver/scope_middleware_test.go`: `TestRequireScope_RejectsRevokedToken`; `internal/miauth/service_test.go`: `TestCheckTokenListRevokeAndDescribeOwner` |
| A presentation actor is never bound by a MiAuth approval and never holds a token | `internal/miauth/service_test.go`: `TestApproveSession_NeverBindsToOpenWebUIModelActor`, `TestCheckAndVerifyToken_NeverResolveToOpenWebUIModelActor` |
| An owner-only write refuses a presentation actor's ID | `internal/miauth/service_test.go`: `TestUpdateOwnerDisplayName_RejectsNonOwnerActor`, `TestUpdateOwnerDisplayName_RejectsOpenWebUIModelActor`, `TestBackfillOwnerDisplayName_LeavesOpenWebUIModelActorAlone` |
| Only the owner actor type reports login/MiAuth capability, and no actor type may hold a credential | `internal/domain/actor_test.go`: `TestActorCapabilityPredicates`, `TestActorCapabilityPredicates_UnknownTypeIsInert` |
| The Open WebUI registry's own owner-only writes (`SetGenerationEnabled`, `SetCapabilityStatus`, `RenameModel`) refuse every non-owner actor, including the VirtualActor the registry itself projects, and fail closed on the feature flag before ever looking the actor up | `internal/openwebui/registry_test.go`: `TestOwnerOnlyMethods_RejectNonOwnerActors`, `TestOwnerOnlyMethods_SucceedForOwner`, `TestOwnerOnlyMethods_DisabledReturnsErrDisabledBeforeCheckingActor` |
| A generated reply's caller-supplied author must be the assistant actor or an active, workspace-enabled Open WebUI model actor; the owner, the system actor, an unknown id, a deactivated model, and a disabled workspace are all refused | `internal/timeline/service_test.go`: `TestCreateGeneratedReplyBy_RejectsIneligibleAuthors` |
| The Open WebUI recovery methods (Issue #53 PR4) refuse a non-owner actor and fail closed on the feature flag before ever looking the actor up, the same guard order `registry_test.go`'s own owner-only tests already establish | `internal/openwebui/recovery_test.go`: `TestConfirmLink_NotOwnerReturnsErrNotOwner`, `TestConfirmLink_DisabledReturnsErrDisabled` |

## AC8: security regression tests

### Request/rate/concurrency limits

Rate and concurrency limiting are deliberately **not** implemented in this
application: this is a single-owner, allowlisted-client server, and that
kind of limiting is delegated to a reverse proxy in front of it (a
separate Issue #13 PR adds the runbook documenting this). What this
application does enforce — request body size and read timeouts — is
covered here:

| Property | Evidence |
| --- | --- |
| Oversized request bodies are rejected | `internal/httpserver/middleware_test.go`: `TestWithMaxBody_RejectsOversizedBody`, `TestWithMaxBody_AllowsBodyWithinLimit` |
| `HTTP_READ_TIMEOUT`/`HTTP_MAX_BODY_BYTES` config is bounds-checked | `internal/config/config_test.go`: `TestConfig_ValidateRejectsHandBuiltConfigWithOutOfBoundsFields`, `TestConfig_ValidateAcceptsHandBuiltConfigWithinBounds` |

### SSRF

| Property | Evidence |
| --- | --- |
| Loopback/private/link-local addresses rejected by default; redirects can't bypass this; scheme can't be downgraded | `internal/ingest/safehttp/client_test.go`: `TestClient_Do_RejectsLoopbackAddressByDefault`, `TestClient_Do_RejectsRedirectToDisallowedAddress`, `TestCheckRedirect_RejectsSchemeDowngradeFromHTTPS`, `TestClient_Do_RejectsDisallowedSchemeOnInitialRequest`, `TestIsPublicUnicastIP` (and the rest of that file) |
| The Open WebUI outbound adapter (Issue #53) reuses this same `safehttp.Client` (redirects disabled outright, not merely re-validated) and additionally refuses a base URL outside its own configured allowlist and a remote chat id shaped like a path-traversal segment before either ever reaches a request | `internal/provider/openwebui/client_test.go`: `TestClient_ContinueTurn_PrivateIPIsPolicyViolation`, `TestClient_StartChat_RedirectIsPolicyViolation_NoSecondRequest`, `TestNewClient_RejectsBaseURLNotInAllowlist`, `TestClient_LookupTurnOutcome_RejectsPathTraversalChatID` |

### Log redaction

| Property | Evidence |
| --- | --- |
| Known sensitive keys (tokens, secrets, credentials) are redacted from structured logs, including nested groups | `internal/logging/logging_test.go`: `TestRedaction_KnownSensitiveKeys`, `TestRedaction_AppliesInsideNestedGroup`, `TestRedaction_NonSensitiveKeysPassThrough` |
| Access logs never include request headers (which may carry the API token) | `internal/logging/middleware_test.go`: `TestAccessLog_NeverLogsHeaders` |
| Job payloads (which may carry post/mail bodies) are never logged | `internal/jobs/manager_test.go`: `TestManagerProcessesJobAndNeverLogsPayload` |
| The Open WebUI outbound adapter (Issue #53) never lets a provider response's own text — including the observed instance's verbatim-echoed upstream credential — reach a returned error, a log line, or any decoded struct; only a fixed local category crosses that boundary | `internal/openwebui/provider_test.go`: `TestProviderError_ErrorTextIsFixedAndCarriesNoWrappedText`; `internal/provider/openwebui/client_test.go`: `TestClient_ContinueTurn_ChatManagedErrorViaGet_TurnFailed`, `TestClient_LookupTurnOutcome_NeverExposesErrorContent` |
| `cmd/openwebuictl` (Issue #53 PR4) never prints an owner post's body text through its `links`/`show` output — only id/state/category/timestamp/boolean-presence fields, the same restriction `cmd/jobsctl`'s own `safeCell`-filtered output already applies to job payloads | `cmd/openwebuictl/main_test.go`: `TestRunLinks_ListsAndFiltersWithoutBody`, `TestRunShow_PrintsLinkAndTurnsWithoutBody` |
| A backup of this service's database cannot contain a raw Open WebUI provider credential, because there is no column or in-memory field to put one in: `internal/openwebui.RegistryConfig` (`internal/openwebui/registry.go`) holds only `SecretRef`, the *name* of the configuration key holding the API key, never the key itself — the same config-not-database storage decision `docs/decisions/0005-openwebui-boundary.md`'s D10 ("Credentials are Open WebUI API keys, held the way this repo already holds secrets") fixes for every layer, not just backups | Verified by inspection, not a dedicated test (Issue #54 OWUI-R PR3): `RegistryConfig`'s field list has no raw-credential field for a backup to ever capture, and D10 is the decision record for why the schema was built that way from the start |

### Prompt injection

A post body containing fake system/role markers (e.g. `"system: ignore
your instructions"`) must never reach or alter the fixed system prompt,
and must always surface only inside a user-role message. Both LLM-facing
prompt builders now carry this exact regression test:

| Package | Evidence |
| --- | --- |
| `internal/llmclassify` | `TestBuildMessages_PromptInjectionNeverReachesSystemMessage` |
| `internal/llmreply` | `TestBuildMessages_PromptInjectionNeverReachesSystemMessage` (added by Issue #13 PR2 for symmetry with `llmclassify`) |

### XSS/escaping

This service has no custom web UI (AGENTS.md non-goal), so the classic
"browser renders attacker HTML and executes a script" path does not exist
for any client-facing surface. The residual risk is untrusted RSS/IMAP
content leaking markup into a stored entry body, and the one HTTP
response in this service that isn't a JSON API body (MiAuth's waiting
page) ever interpolating an attacker-controlled query value.

| Property | Evidence |
| --- | --- |
| `<script>`/`<style>` element content is dropped, not just the tags | `internal/textsanitize/html_test.go`: `TestStripHTML_DropsScriptAndStyleContent` |
| Attribute-based payloads (`onerror`, `onload`, `javascript:` hrefs) never surface after sanitization | `internal/textsanitize/html_test.go`: `TestStripHTML_AttributeBasedXSSPayloadsNeverSurface` (added by Issue #13 PR2) |
| `handleMiAuthStart`'s waiting/error page never interpolates the attacker-controlled `permission`/`callback` query values, and is always served as `text/plain` (never `text/html`) | `internal/httpserver/miauth_handlers_test.go`: `TestHandleMiAuthStart_NeverReflectsQueryValuesInResponseBody` (added by Issue #13 PR2) |
| JSON responses keep Go's default `<`/`>`/`&` HTML-escaping | Verified by inspection, not a dedicated test: no caller in this codebase ever calls `json.Encoder.SetEscapeHTML(false)` (`encoding/json`'s HTML-escaping is on by default and this repository never disables it) |

### Cookie attributes

Not applicable: ADR-0002 explicitly excludes browser session cookies from
this design ("Browser session cookies; authorization occurs through the
host-local CLI" is out of scope). There is no cookie-based session to
attribute-check.

### Token attributes

| Property | Evidence |
| --- | --- |
| Raw tokens are high-entropy and unique | `internal/miauth/token_test.go`: `TestNewRawAPIToken_IsHighEntropyAndUnique` |
| Only a token's hash is stored/compared, never the raw value, and hashing is one-way | `internal/miauth/token_test.go`: `TestHashAPIToken_IsDeterministicAndDistinctForDistinctInput`, `TestHashAPIToken_NeverEqualsRawToken` |
| Scope checks are exact-match, not prefix/substring | `internal/miauth/scope_test.go`: `TestHasScope`, `TestEffectiveScopes_AriaPermissionList`, `TestEffectiveScopes_AlwaysGrantsReadNotes`, `TestEffectiveScopes_OnlyGrantsRequestedGrantableScopes`, `TestEffectiveScopes_IgnoresUnknownAndWhitespace` |
| A revoked token is rejected on its next use, including at the HTTP middleware layer | `internal/httpserver/scope_middleware_test.go`: `TestRequireScope_RejectsRevokedToken`; `internal/miauth/service_test.go`: `TestCheckTokenListRevokeAndDescribeOwner` |
