import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../app_state.dart';
import '../theme.dart';

class ChatsPage extends StatelessWidget {
  const ChatsPage({super.key, required this.onOpenChat});
  final void Function(int groupId) onOpenChat;

  @override
  Widget build(BuildContext context) {
    final state = context.watch<AppState>();
    final ids = [...state.groupIds];
    if (state.timelines.containsKey(0)) {
      ids.insert(0, 0);
    }
    return Scaffold(
      appBar: AppBar(title: const Text('goqbot')),
      body: ids.isEmpty
          ? const Center(child: Text('扫码绑定后，群消息与日志会出现在这里'))
          : ListView.separated(
              itemCount: ids.length,
              separatorBuilder: (_, __) => const Divider(height: 1),
              itemBuilder: (context, i) {
                final gid = ids[i];
                final list = state.timelines[gid] ?? [];
                final last = list.isEmpty ? null : list.last;
                final title = gid == 0 ? '实例日志' : (state.titles[gid] ?? '群 $gid');
                return ListTile(
                  leading: CircleAvatar(
                    backgroundColor: tgBlue,
                    child: Text(
                      title.isNotEmpty ? title.substring(0, 1) : '?',
                      style: const TextStyle(color: Colors.white),
                    ),
                  ),
                  title: Text(title, maxLines: 1, overflow: TextOverflow.ellipsis),
                  subtitle: Text(
                    last == null ? '' : last.displayText,
                    maxLines: 1,
                    overflow: TextOverflow.ellipsis,
                  ),
                  onTap: () => onOpenChat(gid),
                );
              },
            ),
    );
  }
}
