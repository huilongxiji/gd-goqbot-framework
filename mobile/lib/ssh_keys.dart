import 'dart:convert';
import 'dart:typed_data';

import 'package:dartssh2/dartssh2.dart';
import 'package:pinenacl/ed25519.dart';

class GeneratedSSHKeys {
  GeneratedSSHKeys({required this.pem, required this.publicOpenSSH});
  final String pem;
  final String publicOpenSSH;
}

/// Generate OpenSSH Ed25519 keypair for dartssh2 + authorized_keys line.
GeneratedSSHKeys generateMonitorKeys() {
  final sk = SigningKey.generate();
  final pub = Uint8List.fromList(sk.verifyKey.asTypedList);
  final priv = Uint8List.fromList(sk.asTypedList);
  final kp = OpenSSHEd25519KeyPair(pub, priv, 'goqbot-monitor');
  final pem = kp.toPem();
  final blob = base64Encode(kp.toPublicKey().encode());
  final line = 'ssh-ed25519 $blob goqbot-monitor\n';
  return GeneratedSSHKeys(pem: pem, publicOpenSSH: line);
}
