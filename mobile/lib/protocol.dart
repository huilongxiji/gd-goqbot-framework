import 'dart:convert';
import 'dart:typed_data';

class MonFrame {
  MonFrame({required this.type, this.data});
  final String type;
  final Map<String, dynamic>? data;

  static MonFrame? tryDecode(Uint8List body) {
    try {
      final m = jsonDecode(utf8.decode(body)) as Map<String, dynamic>;
      return MonFrame(
        type: m['type'] as String? ?? '',
        data: m['data'] is Map ? Map<String, dynamic>.from(m['data'] as Map) : null,
      );
    } catch (_) {
      return null;
    }
  }

  Uint8List encode() {
    final payload = jsonEncode({'type': type, if (data != null) 'data': data});
    final bytes = utf8.encode(payload);
    final out = BytesBuilder();
    final hdr = ByteData(4)..setUint32(0, bytes.length);
    out.add(hdr.buffer.asUint8List());
    out.add(bytes);
    return out.toBytes();
  }
}

class ChatItem {
  ChatItem({
    required this.id,
    required this.kind, // chat | log
    this.groupId = 0,
    this.userId = 0,
    this.nickname = '',
    this.avatar = '',
    this.message,
    this.self = false,
    this.active = false,
    this.level = 'info',
    this.text = '',
    this.source = '',
    required this.time,
  });

  final String id;
  final String kind;
  final int groupId;
  final int userId;
  final String nickname;
  final String avatar;
  final dynamic message;
  final bool self;
  final bool active;
  final String level;
  final String text;
  final String source;
  final int time;

  String get displayText {
    if (kind == 'log') return text;
    if (message is String) return message as String;
    if (message is List) {
      final buf = StringBuffer();
      for (final p in message as List) {
        if (p is Map && p['type'] == 'text') {
          buf.write(p['data']?['text'] ?? '');
        } else if (p is Map) {
          buf.write('[${p['type']}]');
        }
      }
      return buf.toString();
    }
    return message?.toString() ?? '';
  }
}

class PairingInfo {
  PairingInfo({
    required this.serverId,
    required this.fingerprint,
    this.relayUrl = '',
    this.listen = '',
    this.lan = const [],
    required this.token,
    this.p2pListen = '',
  });

  final String serverId;
  final String fingerprint;
  final String relayUrl;
  final String listen;
  final List<String> lan;
  final String token;
  final String p2pListen;

  factory PairingInfo.fromJson(Map<String, dynamic> j) => PairingInfo(
        serverId: j['server_id'] as String? ?? '',
        fingerprint: j['fingerprint'] as String? ?? '',
        relayUrl: j['relay_url'] as String? ?? '',
        listen: j['listen'] as String? ?? '',
        lan: (j['lan'] as List?)?.map((e) => e.toString()).toList() ?? const [],
        token: j['pairing_token'] as String? ?? '',
        p2pListen: j['p2p_listen'] as String? ?? '',
      );
}
