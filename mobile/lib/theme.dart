import 'package:flutter/material.dart';

const tgBlue = Color(0xFF2AABEE);
const tgBubbleOut = Color(0xFFEFFEDD);
const tgBubbleOutDark = Color(0xFF2B5278);
const tgBubbleIn = Color(0xFFFFFFFF);
const tgBubbleInDark = Color(0xFF182533);
const tgBg = Color(0xFFC4D5E5);
const tgBgDark = Color(0xFF0E1621);
const tgService = Color(0xFF8E8E93);

final tgLightTheme = ThemeData(
  useMaterial3: true,
  colorScheme: ColorScheme.fromSeed(seedColor: tgBlue, brightness: Brightness.light),
  scaffoldBackgroundColor: const Color(0xFFF0F0F0),
  appBarTheme: const AppBarTheme(
    backgroundColor: tgBlue,
    foregroundColor: Colors.white,
    elevation: 0,
  ),
);

final tgDarkTheme = ThemeData(
  useMaterial3: true,
  colorScheme: ColorScheme.fromSeed(seedColor: tgBlue, brightness: Brightness.dark),
  scaffoldBackgroundColor: tgBgDark,
  appBarTheme: const AppBarTheme(
    backgroundColor: Color(0xFF17212B),
    foregroundColor: Colors.white,
    elevation: 0,
  ),
);

Color logColor(String level, Brightness b) {
  switch (level) {
    case 'debug':
      return b == Brightness.dark ? Colors.white24 : Colors.black26;
    case 'warning':
      return const Color(0xFFC9A227);
    case 'error':
      return const Color(0xFFE05A5A);
    default:
      return tgService;
  }
}
