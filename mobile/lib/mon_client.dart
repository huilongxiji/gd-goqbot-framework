import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'package:dartssh2/dartssh2.dart';
import 'package:http/http.dart' as http;

import 'protocol.dart';

typedef FrameHandler = void Function(MonFrame frame);

/// SSH monitor client: LAN first, then relay IP lookup + UDP punch retry.
class MonClient {
  MonClient({required this.onFrame});

  final FrameHandler onFrame;
  SSHClient? _client;
  SSHSession? _session;
  final BytesBuilder _buf = BytesBuilder(copy: false);
  bool connected = false;
  String status = 'idle';

  Future<void> disconnect() async {
    connected = false;
    _session?.close();
    _client?.close();
    _session = null;
    _client = null;
    status = 'disconnected';
  }

  Future<void> connect({
    required PairingInfo info,
    required List<SSHKeyPair> identities,
    required String publicKeyOpenSSH,
    bool pairing = false,
  }) async {
    await disconnect();
    status = 'connecting';
    final failures = <String>[];

    for (final ep in info.lan) {
      try {
        await _connectHost(ep, identities, pairing, info, publicKeyOpenSSH);
        return;
      } catch (e) {
        failures.add('$ep: $e');
      }
    }

    if (info.relayUrl.isNotEmpty && info.serverId.isNotEmpty) {
      status = 'relay lookup';
      try {
        final base = info.relayUrl.replaceAll(RegExp(r'/$'), '');
        final resp = await http
            .get(Uri.parse('$base/peers/${info.serverId}'))
            .timeout(const Duration(seconds: 8));
        if (resp.statusCode == 200) {
          final j = jsonDecode(resp.body) as Map<String, dynamic>;
          final ip = j['ip'] as String? ?? '';
          final port = j['port'] as int? ?? 0;
          if (ip.isNotEmpty && port > 0) {
            await _udpPunch(ip, port);
            await _connectHost('$ip:$port', identities, pairing, info, publicKeyOpenSSH);
            return;
          }
        }
      } catch (e) {
        failures.add('relay: $e');
      }
    }

    status = 'failed';
    throw Exception('connect failed: ${failures.join('; ')}');
  }

  Future<void> _udpPunch(String ip, int port) async {
    try {
      final sock = await RawDatagramSocket.bind(InternetAddress.anyIPv4, 0);
      final payload = utf8.encode(jsonEncode({'type': 'punch'}));
      final target = InternetAddress(ip);
      for (var i = 0; i < 5; i++) {
        sock.send(payload, target, port);
        await Future.delayed(const Duration(milliseconds: 200));
      }
      sock.close();
    } catch (_) {}
  }

  Future<void> _connectHost(
    String endpoint,
    List<SSHKeyPair> identities,
    bool pairing,
    PairingInfo info,
    String publicKeyOpenSSH,
  ) async {
    final parts = endpoint.split(':');
    if (parts.length < 2) throw Exception('bad endpoint $endpoint');
    final host = parts.sublist(0, parts.length - 1).join(':');
    final port = int.parse(parts.last);
    status = 'tcp $endpoint';

    final socket = await SSHSocket.connect(host, port, timeout: const Duration(seconds: 8));
    final client = SSHClient(
      socket,
      username: 'goqbot',
      identities: identities,
    );

    late SSHSession session;
    // dartssh2 对自定义 subsystem 无返回 Session 的公开 API；走 shell，官机侧已接受 shell
    session = await client.shell(pty: null);

    _client = client;
    _session = session;
    connected = true;
    status = 'connected $endpoint';
    session.stdout.listen(_onBytes);

    if (pairing) {
      await sendFrame(MonFrame(type: 'pair', data: {
        'token': info.token,
        'public_key': publicKeyOpenSSH,
      }));
    } else {
      await sendFrame(MonFrame(type: 'subscribe'));
    }
  }

  void _onBytes(List<int> chunk) {
    _buf.add(Uint8List.fromList(chunk));
    while (true) {
      final bytes = _buf.toBytes();
      if (bytes.length < 4) {
        _buf.clear();
        _buf.add(bytes);
        return;
      }
      final n = ByteData.sublistView(bytes).getUint32(0);
      if (n > 4 << 20 || bytes.length < 4 + n) {
        _buf.clear();
        _buf.add(bytes);
        return;
      }
      final body = bytes.sublist(4, 4 + n);
      final rest = bytes.sublist(4 + n);
      _buf.clear();
      if (rest.isNotEmpty) _buf.add(rest);
      final frame = MonFrame.tryDecode(Uint8List.fromList(body));
      if (frame != null) onFrame(frame);
    }
  }

  Future<void> sendFrame(MonFrame frame) async {
    final s = _session;
    if (s == null) return;
    s.write(frame.encode());
  }

  Future<void> sendGroup(int groupId, String text) async {
    await sendFrame(MonFrame(type: 'send_group', data: {
      'group_id': groupId,
      'message': text,
    }));
  }
}
