import 'dart:convert';

import 'package:dartssh2/dartssh2.dart';
import 'package:flutter/foundation.dart';
import 'package:flutter_secure_storage/flutter_secure_storage.dart';

import 'mon_client.dart';
import 'protocol.dart';
import 'ssh_keys.dart';

const _kPem = 'ssh_private_pem';
const _kPub = 'ssh_public_openssh';
const _kPairing = 'pairing_json';

class AppState extends ChangeNotifier {
  AppState() {
    _load();
  }

  final _storage = const FlutterSecureStorage();
  MonClient? client;
  PairingInfo? pairing;
  String? privatePem;
  String? publicOpenSSH;
  String status = 'idle';
  final Map<int, List<ChatItem>> timelines = {};
  final Map<int, String> titles = {};
  int? activeGroupId;

  List<int> get groupIds {
    final ids = timelines.keys.where((e) => e != 0).toList()..sort();
    return ids;
  }

  Future<void> _load() async {
    privatePem = await _storage.read(key: _kPem);
    publicOpenSSH = await _storage.read(key: _kPub);
    final pj = await _storage.read(key: _kPairing);
    if (pj != null) {
      try {
        pairing = PairingInfo.fromJson(jsonDecode(pj) as Map<String, dynamic>);
      } catch (_) {}
    }
    if (privatePem == null) {
      await _generateKeys();
    }
    notifyListeners();
  }

  Future<void> _generateKeys() async {
    final keys = generateMonitorKeys();
    privatePem = keys.pem;
    publicOpenSSH = keys.publicOpenSSH;
    await _storage.write(key: _kPem, value: privatePem);
    await _storage.write(key: _kPub, value: publicOpenSSH);
  }

  Future<void> applyPairingJson(String raw) async {
    final info = PairingInfo.fromJson(jsonDecode(raw) as Map<String, dynamic>);
    pairing = info;
    await _storage.write(key: _kPairing, value: raw);
    notifyListeners();
    await connect(pairingMode: true);
  }

  List<SSHKeyPair> _identities() {
    final pem = privatePem;
    if (pem == null) return [];
    return SSHKeyPair.fromPem(pem);
  }

  void _onFrame(MonFrame f) {
    final now = DateTime.now().millisecondsSinceEpoch;
    if (f.type == 'chat' && f.data != null) {
      final d = f.data!;
      final gid = (d['group_id'] as num?)?.toInt() ?? 0;
      final item = ChatItem(
        id: '$now-${timelines[gid]?.length ?? 0}',
        kind: 'chat',
        groupId: gid,
        userId: (d['user_id'] as num?)?.toInt() ?? 0,
        nickname: d['nickname'] as String? ?? '',
        avatar: d['avatar'] as String? ?? '',
        message: d['message'],
        self: d['self'] == true,
        active: d['active'] == true,
        time: (d['time'] as num?)?.toInt() ?? (now ~/ 1000),
      );
      timelines.putIfAbsent(gid, () => []).add(item);
      if (gid != 0 && (titles[gid] == null || titles[gid]!.isEmpty)) {
        titles[gid] = '群 $gid';
      }
      notifyListeners();
    } else if (f.type == 'log' && f.data != null) {
      final d = f.data!;
      final gid = (d['group_id'] as num?)?.toInt() ?? 0;
      final item = ChatItem(
        id: '$now-log-${timelines[gid]?.length ?? 0}',
        kind: 'log',
        groupId: gid,
        level: d['level'] as String? ?? 'info',
        text: d['text'] as String? ?? '',
        source: d['source'] as String? ?? '',
        time: (d['time'] as num?)?.toInt() ?? (now ~/ 1000),
      );
      timelines.putIfAbsent(gid, () => []).add(item);
      notifyListeners();
    } else if (f.type == 'hello') {
      status = 'hello ${f.data?['server_id'] ?? ''}';
      notifyListeners();
    }
  }

  Future<void> connect({bool pairingMode = false}) async {
    final info = pairing;
    if (info == null) {
      status = 'no pairing';
      notifyListeners();
      return;
    }
    client ??= MonClient(onFrame: _onFrame);
    status = 'connecting…';
    notifyListeners();
    try {
      await client!.connect(
        info: info,
        identities: _identities(),
        publicKeyOpenSSH: publicOpenSSH ?? '',
        pairing: pairingMode,
      );
      status = client!.status;
      if (pairingMode) {
        await Future.delayed(const Duration(milliseconds: 500));
        await client!.connect(
          info: info,
          identities: _identities(),
          publicKeyOpenSSH: publicOpenSSH ?? '',
          pairing: false,
        );
        status = client!.status;
      }
    } catch (e) {
      status = 'error: $e';
    }
    notifyListeners();
  }

  Future<void> send(String text) async {
    final gid = activeGroupId;
    if (gid == null || gid == 0 || client == null) return;
    await client!.sendGroup(gid, text);
  }
}
