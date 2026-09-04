import 'package:misskey_dart/misskey_dart.dart';
import 'package:test/test.dart';

import 'util/test_client.dart';

void main() {
  test('POST /api/notes/conversation returns the oldest-first ancestor chain', () async {
    final misskey = buildTestClient();
    final root = await misskey.notes.create(
      NotesCreateRequest(text: 'conversation root ${DateTime.now().microsecondsSinceEpoch}'),
    );
    final child = await misskey.notes.create(
      NotesCreateRequest(text: 'conversation child ${DateTime.now().microsecondsSinceEpoch}', replyId: root!.id),
    );
    final grandchild = await misskey.notes.create(
      NotesCreateRequest(text: 'conversation grandchild ${DateTime.now().microsecondsSinceEpoch}', replyId: child!.id),
    );

    final ancestors = (await misskey.notes.conversation(
      NotesConversationRequest(noteId: grandchild!.id),
    )).toList();

    expect(ancestors.map((n) => n.id), orderedEquals([root.id, child.id]));
  });
}
