import 'dart:io';

import 'package:misskey_dart/misskey_dart.dart';

/// The running server's API base, e.g. `http://localhost:18080/api/`.
/// scripts/run-contract-tests.sh sets TEST_API_URL after starting
/// bin/server and bin/fakemisskey.
Uri testApiUrl() => Uri.parse(_requiredEnv('TEST_API_URL'));

/// An authenticated [Misskey] client using the local API token
/// scripts/run-contract-tests.sh obtained by driving a real (fake-
/// upstream-backed) MiAuth flow before invoking `dart test`. apiUrl is
/// passed explicitly so misskey_dart never derives it from serverUrl's
/// path segments; see docs/compat/aria-v1.5.11.md's "Issue #7
/// implementation notes" for why this substitutes for a real Aria E2E
/// run.
Misskey buildTestClient() {
  final apiUrl = testApiUrl();
  return Misskey(
    token: _requiredEnv('TEST_TOKEN'),
    serverUrl: apiUrl,
    apiUrl: apiUrl,
  );
}

/// An anonymous [Misskey] client (no `i` token), for endpoints Aria calls
/// before authenticating (`/api/meta`).
Misskey buildAnonymousClient() {
  final apiUrl = testApiUrl();
  return Misskey(serverUrl: apiUrl, apiUrl: apiUrl);
}

String _requiredEnv(String name) {
  final value = Platform.environment[name];
  if (value == null || value.isEmpty) {
    throw StateError(
      '$name must be set; run these tests through '
      'scripts/run-contract-tests.sh rather than `dart test` directly.',
    );
  }
  return value;
}
