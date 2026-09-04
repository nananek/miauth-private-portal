import 'package:misskey_dart/misskey_dart.dart';
import 'package:test/test.dart';

import 'util/test_client.dart';

void main() {
  test('POST /api/i decodes as MeDetailed for the logged-in owner', () async {
    final misskey = buildTestClient();

    final MeDetailed me = await misskey.i.i();

    expect(me.id, isNotEmpty);
    expect(me.username, isNotEmpty);
    // Single-owner deployment: the only login-capable actor is
    // administrator-equivalent by construction (see meDetailed's doc
    // comment in internal/httpserver/noteapi_wire.go).
    expect(me.isModerator, isTrue);
    expect(me.isAdmin, isTrue);
    expect(me.notesCount, greaterThanOrEqualTo(0));
  });

  test('notesCount increases by exactly one after posting a note', () async {
    final misskey = buildTestClient();

    final before = await misskey.i.i();
    await misskey.notes.create(NotesCreateRequest(text: 'notesCount probe ${DateTime.now().microsecondsSinceEpoch}'));
    final after = await misskey.i.i();

    expect(after.notesCount, equals(before.notesCount + 1));
  });
}
