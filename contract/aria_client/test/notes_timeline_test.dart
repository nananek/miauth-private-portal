import 'package:misskey_dart/misskey_dart.dart';
import 'package:test/test.dart';

import 'util/test_client.dart';

void main() {
  test(
    'POST /api/notes/timeline returns newest-first and paginates with untilId',
    () async {
      final misskey = buildTestClient();
      await misskey.notes.create(
        NotesCreateRequest(
          text: 'timeline older ${DateTime.now().microsecondsSinceEpoch}',
        ),
      );
      final newer = await misskey.notes.create(
        NotesCreateRequest(
          text: 'timeline newer ${DateTime.now().microsecondsSinceEpoch}',
        ),
      );

      final firstPage = (await misskey.notes.homeTimeline(
        NotesTimelineRequest(limit: 1),
      )).toList();
      expect(firstPage, hasLength(1));
      expect(firstPage.first.id, equals(newer!.id));

      final secondPage = (await misskey.notes.homeTimeline(
        NotesTimelineRequest(limit: 1, untilId: firstPage.first.id),
      )).toList();
      expect(secondPage, hasLength(1));
      expect(secondPage.first.id, isNot(equals(firstPage.first.id)));
      expect(
        secondPage.first.createdAt.isAfter(firstPage.first.createdAt),
        isFalse,
        reason:
            'untilId must page to strictly older (or equal-timestamp, tie-broken) notes',
      );
    },
  );

  test(
    'POST /api/notes/timeline with an unknown untilId returns an empty page',
    () async {
      final misskey = buildTestClient();

      final page = await misskey.notes.homeTimeline(
        NotesTimelineRequest(untilId: 'does-not-exist'),
      );

      expect(page, isEmpty);
    },
  );
}
