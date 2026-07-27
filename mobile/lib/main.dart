import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import 'app_state.dart';
import 'theme.dart';
import 'screens/chats_page.dart';
import 'screens/chat_page.dart';
import 'screens/bind_page.dart';

void main() {
  WidgetsFlutterBinding.ensureInitialized();
  runApp(const GoqbotMonitorApp());
}

class GoqbotMonitorApp extends StatelessWidget {
  const GoqbotMonitorApp({super.key});

  @override
  Widget build(BuildContext context) {
    return ChangeNotifierProvider(
      create: (_) => AppState(),
      child: MaterialApp(
        title: 'goqbot monitor',
        debugShowCheckedModeBanner: false,
        theme: tgLightTheme,
        darkTheme: tgDarkTheme,
        themeMode: ThemeMode.system,
        home: const HomeShell(),
      ),
    );
  }
}

class HomeShell extends StatefulWidget {
  const HomeShell({super.key});

  @override
  State<HomeShell> createState() => _HomeShellState();
}

class _HomeShellState extends State<HomeShell> {
  int _tab = 0;

  @override
  Widget build(BuildContext context) {
    final state = context.watch<AppState>();
    return Scaffold(
      body: IndexedStack(
        index: _tab,
        children: [
          ChatsPage(onOpenChat: (gid) {
            state.activeGroupId = gid;
            Navigator.of(context).push(MaterialPageRoute(builder: (_) => ChatPage(groupId: gid)));
          }),
          const BindPage(),
        ],
      ),
      bottomNavigationBar: NavigationBar(
        selectedIndex: _tab,
        onDestinationSelected: (i) => setState(() => _tab = i),
        destinations: const [
          NavigationDestination(icon: Icon(Icons.chat_bubble_outline), label: 'Chats'),
          NavigationDestination(icon: Icon(Icons.qr_code_scanner), label: 'Bind'),
        ],
      ),
      floatingActionButton: _tab == 0
          ? FloatingActionButton.extended(
              onPressed: () => state.connect(pairingMode: false),
              label: Text(state.status),
              icon: const Icon(Icons.link),
            )
          : null,
    );
  }
}
