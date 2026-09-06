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

| Bullet | Evidence |
| --- | --- |
| Unapproved session never yields a token | `internal/miauth/service_test.go`: `TestApproveAndRejectUnavailableSessions`, `TestRejectAndListPendingSessions` |
| Expired local MiAuth session cannot resume | `internal/miauth/service_test.go`: `TestStartLocalSession_ExpiredCannotResume` |
| Replayed `/api/miauth/{session}/check` consume is rejected | `internal/httpserver/miauth_handlers_test.go`: `TestHandleMiAuthCheck_ApprovalSuccessAndReplay`, `TestHandleMiAuthCheck_ConcurrentCallsHaveExactlyOneWinner` |
| Wrong scope is rejected | `internal/httpserver/scope_middleware_test.go`: `TestRequireScope_RejectsInsufficientScope` |
| Revoked token is rejected | `internal/httpserver/scope_middleware_test.go`: `TestRequireScope_RejectsRevokedToken`; `internal/miauth/service_test.go`: `TestCheckTokenListRevokeAndDescribeOwner` |

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

### Log redaction

| Property | Evidence |
| --- | --- |
| Known sensitive keys (tokens, secrets, credentials) are redacted from structured logs, including nested groups | `internal/logging/logging_test.go`: `TestRedaction_KnownSensitiveKeys`, `TestRedaction_AppliesInsideNestedGroup`, `TestRedaction_NonSensitiveKeysPassThrough` |
| Access logs never include request headers (which may carry the API token) | `internal/logging/middleware_test.go`: `TestAccessLog_NeverLogsHeaders` |
| Job payloads (which may carry post/mail bodies) are never logged | `internal/jobs/manager_test.go`: `TestManagerProcessesJobAndNeverLogsPayload` |

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
