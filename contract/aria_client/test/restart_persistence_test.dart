import 'package:misskey_dart/misskey_dart.dart';
import 'package:test/test.dart';

import 'util/test_client.dart';

/// Issue #13 AC1/AC2: scripts/run-contract-tests.sh creates one note,
/// restarts bin/server against the same DB_PATH, and passes this note's
/// id/text here so this suite proves — through the real pinned decoder,
/// against a server that has actually been killed and relaunched, not
/// merely a database file reopened in-process — that a note survives a
/// full process restart. The local API token obtained before the
/// restart is reused unchanged: tokens are stored (hashed) in the same
/// SQLite database and must remain valid across a restart too.
void main() {
  test(
    'POST /api/notes/show finds a note created before a server restart',
    () async {
      final misskey = buildTestClient();
      final noteId = requiredEnv('TEST_PRE_RESTART_NOTE_ID');
      final noteText = requiredEnv('TEST_PRE_RESTART_NOTE_TEXT');

      final shown = await misskey.notes.show(NotesShowRequest(noteId: noteId));

      expect(shown.id, equals(noteId));
      expect(shown.text, equals(noteText));
    },
  );
}
