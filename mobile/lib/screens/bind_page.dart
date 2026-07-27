import 'package:flutter/material.dart';
import 'package:mobile_scanner/mobile_scanner.dart';
import 'package:provider/provider.dart';

import '../app_state.dart';

class BindPage extends StatefulWidget {
  const BindPage({super.key});

  @override
  State<BindPage> createState() => _BindPageState();
}

class _BindPageState extends State<BindPage> {
  final _manual = TextEditingController();
  bool _scanning = false;

  @override
  void dispose() {
    _manual.dispose();
    super.dispose();
  }

  Future<void> _apply(String raw) async {
    final state = context.read<AppState>();
    await state.applyPairingJson(raw.trim());
    if (mounted) {
      ScaffoldMessenger.of(context).showSnackBar(SnackBar(content: Text(state.status)));
    }
  }

  @override
  Widget build(BuildContext context) {
    final state = context.watch<AppState>();
    return Scaffold(
      appBar: AppBar(title: const Text('绑定官机')),
      body: ListView(
        padding: const EdgeInsets.all(16),
        children: [
          Text('状态: ${state.status}'),
          const SizedBox(height: 8),
          if (state.pairing != null)
            Text('已绑定 server_id=${state.pairing!.serverId}\nfp=${state.pairing!.fingerprint}'),
          const SizedBox(height: 16),
          FilledButton.icon(
            onPressed: () => setState(() => _scanning = !_scanning),
            icon: const Icon(Icons.qr_code_scanner),
            label: Text(_scanning ? '关闭扫码' : '扫描服务端二维码'),
          ),
          if (_scanning) ...[
            const SizedBox(height: 12),
            SizedBox(
              height: 280,
              child: ClipRRect(
                borderRadius: BorderRadius.circular(12),
                child: MobileScanner(
                  onDetect: (capture) {
                    final bars = capture.barcodes;
                    if (bars.isEmpty) return;
                    final raw = bars.first.rawValue;
                    if (raw == null || raw.isEmpty) return;
                    setState(() => _scanning = false);
                    _apply(raw);
                  },
                ),
              ),
            ),
          ],
          const SizedBox(height: 24),
          const Text('或粘贴二维码 JSON：'),
          TextField(
            controller: _manual,
            maxLines: 6,
            decoration: const InputDecoration(border: OutlineInputBorder()),
          ),
          const SizedBox(height: 8),
          FilledButton(
            onPressed: () => _apply(_manual.text),
            child: const Text('绑定并连接'),
          ),
          const SizedBox(height: 12),
          OutlinedButton(
            onPressed: () => state.connect(pairingMode: false),
            child: const Text('重新连接'),
          ),
        ],
      ),
    );
  }
}
