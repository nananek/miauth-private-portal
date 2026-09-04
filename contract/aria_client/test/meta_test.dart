import 'package:test/test.dart';

import 'util/test_client.dart';

void main() {
  test('POST /api/meta decodes anonymously and advertises miauth', () async {
    final misskey = buildAnonymousClient();

    final meta = await misskey.meta();

    expect(meta.features?.miauth, isTrue);
    expect(meta.uri, isNotNull);
  });
}
