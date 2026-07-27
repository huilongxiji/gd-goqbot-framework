import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../app_state.dart';
import '../protocol.dart';
import '../theme.dart';

class ChatPage extends StatefulWidget {
  const ChatPage({super.key, required this.groupId});
  final int groupId;

  @override
  State<ChatPage> createState() => _ChatPageState();
}

class _ChatPageState extends State<ChatPage> {
  final _ctrl = TextEditingController();
  final _scroll = ScrollController();

  @override
  void dispose() {
    _ctrl.dispose();
    _scroll.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final state = context.watch<AppState>();
    final items = state.timelines[widget.groupId] ?? [];
    final title = widget.groupId == 0 ? '实例日志' : (state.titles[widget.groupId] ?? '群 ${widget.groupId}');
    final dark = Theme.of(context).brightness == Brightness.dark;

    return Scaffold(
      appBar: AppBar(title: Text(title)),
      body: Column(
        children: [
          Expanded(
            child: Container(
              color: dark ? tgBgDark : tgBg,
              child: ListView.builder(
                controller: _scroll,
                padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 12),
                itemCount: items.length,
                itemBuilder: (context, i) => _buildItem(context, items[i], dark),
              ),
            ),
          ),
          if (widget.groupId != 0) _inputBar(state),
        ],
      ),
    );
  }

  Widget _buildItem(BuildContext context, ChatItem item, bool dark) {
    if (item.kind == 'log') {
      final prefix = item.source.isEmpty ? '' : '[${item.source}] ';
      return Padding(
        padding: const EdgeInsets.symmetric(vertical: 6),
        child: Center(
          child: Text(
            '$prefix${item.text}',
            textAlign: TextAlign.center,
            style: TextStyle(
              fontSize: 13,
              color: logColor(item.level, Theme.of(context).brightness),
            ),
          ),
        ),
      );
    }

    final mine = item.self;
    final bubbleColor = mine
        ? (dark ? tgBubbleOutDark : tgBubbleOut)
        : (dark ? tgBubbleInDark : tgBubbleIn);
    return Align(
      alignment: mine ? Alignment.centerRight : Alignment.centerLeft,
      child: ConstrainedBox(
        constraints: BoxConstraints(maxWidth: MediaQuery.of(context).size.width * 0.78),
        child: Container(
          margin: const EdgeInsets.symmetric(vertical: 4),
          padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 8),
          decoration: BoxDecoration(
            color: bubbleColor,
            borderRadius: BorderRadius.only(
              topLeft: const Radius.circular(14),
              topRight: const Radius.circular(14),
              bottomLeft: Radius.circular(mine ? 14 : 4),
              bottomRight: Radius.circular(mine ? 4 : 14),
            ),
          ),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              if (!mine && item.nickname.isNotEmpty)
                Text(item.nickname, style: TextStyle(color: tgBlue, fontSize: 13, fontWeight: FontWeight.w600)),
              Text(item.displayText, style: TextStyle(color: dark ? Colors.white : Colors.black87, height: 1.3)),
              const SizedBox(height: 2),
              Row(
                mainAxisSize: MainAxisSize.min,
                children: [
                  if (mine && item.active)
                    const Padding(
                      padding: EdgeInsets.only(right: 6),
                      child: Text('主动', style: TextStyle(fontSize: 10, color: Colors.orange)),
                    ),
                  Text(
                    _fmtTime(item.time),
                    style: TextStyle(fontSize: 11, color: dark ? Colors.white54 : Colors.black45),
                  ),
                ],
              ),
            ],
          ),
        ),
      ),
    );
  }

  String _fmtTime(int unix) {
    final t = DateTime.fromMillisecondsSinceEpoch(unix * 1000);
    final h = t.hour.toString().padLeft(2, '0');
    final m = t.minute.toString().padLeft(2, '0');
    return '$h:$m';
  }

  Widget _inputBar(AppState state) {
    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(8, 6, 8, 8),
        child: Row(
          children: [
            Expanded(
              child: TextField(
                controller: _ctrl,
                decoration: InputDecoration(
                  hintText: 'Message',
                  filled: true,
                  fillColor: Theme.of(context).cardColor,
                  border: OutlineInputBorder(borderRadius: BorderRadius.circular(22), borderSide: BorderSide.none),
                  contentPadding: const EdgeInsets.symmetric(horizontal: 16, vertical: 10),
                ),
                minLines: 1,
                maxLines: 4,
              ),
            ),
            const SizedBox(width: 8),
            CircleAvatar(
              backgroundColor: tgBlue,
              child: IconButton(
                icon: const Icon(Icons.send, color: Colors.white, size: 20),
                onPressed: () async {
                  final t = _ctrl.text.trim();
                  if (t.isEmpty) return;
                  _ctrl.clear();
                  await state.send(t);
                },
              ),
            ),
          ],
        ),
      ),
    );
  }
}
