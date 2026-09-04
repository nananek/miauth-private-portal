import 'package:misskey_dart/misskey_dart.dart';
import 'package:test/test.dart';

import 'util/test_client.dart';

void main() {
  test('POST /api/notes/create decodes the created root note', () async {
    final misskey = buildTestClient();
    final text = 'root note ${DateTime.now().microsecondsSinceEpoch}';

    final created = await misskey.notes.create(NotesCreateRequest(text: text));

    expect(created, isNotNull);
    expect(created!.id, isNotEmpty);
    expect(created.text, equals(text));
    expect(created.userId, equals(created.user.id));
    expect(created.replyId, isNull);
  });

  test('POST /api/notes/create with replyId decodes the created reply', () async {
    final misskey = buildTestClient();
    final rootText = 'reply parent ${DateTime.now().microsecondsSinceEpoch}';
    final root = await misskey.notes.create(NotesCreateRequest(text: rootText));

    final replyText = 'reply body ${DateTime.now().microsecondsSinceEpoch}';
    final reply = await misskey.notes.create(
      NotesCreateRequest(text: replyText, replyId: root!.id),
    );

    expect(reply, isNotNull);
    expect(reply!.replyId, equals(root.id));
    expect(reply.text, equals(replyText));
  });

  test('POST /api/notes/create rejects an unknown replyId as NO_SUCH_NOTE', () async {
    final misskey = buildTestClient();

    await expectLater(
      misskey.notes.create(NotesCreateRequest(text: 'orphan reply', replyId: 'does-not-exist')),
      throwsA(
        isA<MisskeyException>().having((e) => e.code, 'code', 'NO_SUCH_NOTE'),
      ),
    );
  });
}
