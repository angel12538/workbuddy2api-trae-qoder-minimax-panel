// Package ideauth 提供本地 IDE（Qoder / ZCode / Trae）账号凭证的读取、备份、
// 覆盖与切换能力。各 IDE 的加密方案不同，此处逐一封装：
//
//   Qoder : auth.v1.dat = "v10" + AES-256-GCM(iv12|ct|tag16)
//           key = DPAPI(Local State.os_crypt.encrypted_key)
//   ZCode : credentials.json = AES-256-GCM，key = sha256(ZCODE_CREDENTIAL_SECRET
//           或 "zcode-credential-fallback:{platform}:{homedir}:{username}")
//   Trae  : %APPDATA%\TRAE SOLO CN，由 Trae 助手侧负责（profile 隔离）
package ideauth
