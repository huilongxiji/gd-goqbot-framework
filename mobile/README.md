# goqbot_monitor (Flutter)

Telegram 风格安卓监控客户端，经 SSH 连接 goqbot 内嵌 `sshmon`。

## 环境

- Flutter 3.19+ / Dart 3.3+
- Android SDK

## 运行

```bash
cd mobile
flutter create . --platforms=android
flutter pub get
flutter run
```

首次 `flutter create .` 会补齐 `android/` 工程文件，不会覆盖 `lib/` 与 `pubspec.yaml`。

## 使用

1. 官机开启 `sshmon.enabled: true`，终端打印配对二维码 / JSON  
2. App「Bind」页扫码或粘贴 JSON  
3. 完成公钥绑定后自动订阅事件  
4. Chats 中按群查看气泡消息与居中灰字日志，并发送文本  

连接顺序：扫码中的 LAN → 失败则查中继 IP → UDP punch → 再连 SSH。
