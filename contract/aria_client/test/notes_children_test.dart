import 'package:misskey_dart/misskey_dart.dart';
import 'package:test/test.dart';

import 'util/test_client.dart';

void main() {
  test('POST /api/notes/children returns direct replies oldest-first', () async {
    final misskey = buildTestClient();
    final root = await misskey.notes.create(
      NotesCreateRequest(text: 'children root ${DateTime.now().microsecondsSinceEpoch}'),
    );
    final first = await misskey.notes.create(
      NotesCreateRequest(text: 'children first ${DateTime.now().microsecondsSinceEpoch}', replyId: root!.id),
    );
    final second = await misskey.notes.create(
      NotesCreateRequest(text: 'children second ${DateTime.now().microsecondsSinceEpoch}', replyId: root.id),
    );

    final children = (await misskey.notes.children(
      NotesChildrenRequest(noteId: root.id),
    )).toList();

    expect(children.map((n) => n.id), orderedEquals([first!.id, second!.id]));
  });

  test('POST /api/notes/children on an unknown noteId decodes as MisskeyException NO_SUCH_NOTE', () async {
    final misskey = buildTestClient();

    await expectLater(
      misskey.notes.children(NotesChildrenRequest(noteId: 'does-not-exist')),
      throwsA(
        isA<MisskeyException>().having((e) => e.code, 'code', 'NO_SUCH_NOTE'),
      ),
    );
  });
}
