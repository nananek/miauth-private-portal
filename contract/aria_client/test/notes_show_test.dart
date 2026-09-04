import 'package:misskey_dart/misskey_dart.dart';
import 'package:test/test.dart';

import 'util/test_client.dart';

void main() {
  test('POST /api/notes/show decodes a previously created note', () async {
    final misskey = buildTestClient();
    final text = 'show me ${DateTime.now().microsecondsSinceEpoch}';
    final created = await misskey.notes.create(NotesCreateRequest(text: text));

    final shown = await misskey.notes.show(NotesShowRequest(noteId: created!.id));

    expect(shown.id, equals(created.id));
    expect(shown.text, equals(text));
  });

  test('POST /api/notes/show on an unknown noteId decodes as MisskeyException NO_SUCH_NOTE', () async {
    final misskey = buildTestClient();

    await expectLater(
      misskey.notes.show(NotesShowRequest(noteId: 'does-not-exist')),
      throwsA(
        isA<MisskeyException>()
            .having((e) => e.code, 'code', 'NO_SUCH_NOTE')
            .having((e) => e.id, 'id', isNotEmpty)
            .having((e) => e.message, 'message', isNotEmpty),
      ),
    );
  });
}
