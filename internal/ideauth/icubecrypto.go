package ideauth

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Trae（iCube 内核）本地凭据解密。
//
// storage.json 里的 `iCubeAuthInfo://*` 值不是明文，而是客户端 byteCrypto
// （out-build/vs/base/common/byteCrypto.js）加密出的 blob。算法：
//
//	密文 = header(6) || seed(32) || AES-128-CBC(
//	          key  = derive(seed)[0:16],
//	          iv   = derive(seed)[16:32],
//	          data = SHA512(plain) (64) || plain )
//
//	derive(seed) = SHA512( SHA512(seed) || xorMask )。header = "tc" 0x05 0x10 0x00 0x00。
//
// 其中 xorMask 是两个 64 字节内嵌常量逐字节异或的结果。解密后校验前 64 字节
// 是否等于明文段的 SHA-512。
//
// 这让我们能直接读出本机 Trae 的真实登录身份（`iCubeAuthInfo://icube.cloudide`
// 解密后是 {"token":"<JWT>", ...}，JWT payload 的 data.id 即当前 uid），
// 不再依赖会滞后的 icube_gtm 埋点缓存。

const (
	icubeDigestLen = 64  // Gh
	icubeSeedLen   = 32  // d2
	icubeHeaderLen = 6   // P1
	icubeKeySize   = 16  // AES-128
	icubeIVSize    = 16
)

// iCubeAuthInfo://icube.cloudide 的键名（登录会话，含 token）。
const traeCloudideKey = "iCubeAuthInfo://icube.cloudide"

var icubeConstA = [64]byte{82, 9, 106, 213, 48, 54, 165, 56, 191, 64, 163, 158, 129, 243, 215, 251, 124, 227, 57, 130, 155, 47, 255, 135, 52, 142, 67, 68, 196, 222, 233, 203, 84, 123, 148, 50, 166, 194, 35, 61, 238, 76, 149, 11, 66, 250, 195, 78, 8, 46, 161, 102, 40, 217, 36, 178, 118, 91, 162, 73, 109, 139, 209, 37}

var icubeConstB = [64]byte{31, 221, 168, 51, 136, 7, 199, 49, 177, 18, 16, 89, 39, 128, 236, 95, 96, 81, 127, 169, 25, 181, 74, 13, 45, 229, 122, 159, 147, 201, 156, 239, 160, 224, 59, 77, 174, 42, 245, 176, 200, 235, 187, 60, 131, 83, 153, 97, 23, 43, 4, 126, 186, 119, 214, 38, 225, 105, 20, 99, 85, 33, 12, 125}

// icubeXorMask 返回两个内嵌常量逐字节异或得到的 64 字节掩码。
func icubeXorMask() [64]byte {
	var m [64]byte
	for i := range m {
		m[i] = icubeConstA[i] ^ icubeConstB[i]
	}
	return m
}

// icubeDeriveKey 复现 byteCrypto 的密钥派生，返回 AES key 与 IV。
func icubeDeriveKey(seed []byte) (key, iv []byte) {
	s := make([]byte, icubeDigestLen+icubeDigestLen) // 128
	h1 := sha512.Sum512(seed)
	mask := icubeXorMask()
	copy(s[0:64], h1[:])
	copy(s[64:128], mask[:])
	h2 := sha512.Sum512(s)
	copy(s[0:64], h2[:])
	return s[0:icubeKeySize], s[icubeKeySize : icubeKeySize+icubeIVSize]
}

// pkcs7Unpad 去掉 WebCrypto AES-CBC 默认添加的 PKCS#7 填充。
func pkcs7Unpad(b []byte) ([]byte, error) {
	n := len(b)
	if n == 0 || n%aes.BlockSize != 0 {
		return nil, fmt.Errorf("invalid padded length %d", n)
	}
	pad := int(b[n-1])
	if pad == 0 || pad > aes.BlockSize || pad > n {
		return nil, fmt.Errorf("invalid padding byte %d", pad)
	}
	for i := n - pad; i < n; i++ {
		if int(b[i]) != pad {
			return nil, fmt.Errorf("invalid padding")
		}
	}
	return b[:n-pad], nil
}

// DecryptICubeBlob 解密一个 storage.json 里的 `iCubeAuthInfo://*` 密文串。
func DecryptICubeBlob(b64 string) ([]byte, error) {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return nil, fmt.Errorf("empty blob")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	if len(raw) < icubeHeaderLen+icubeSeedLen {
		return nil, fmt.Errorf("blob too short (%d)", len(raw))
	}
	// header：AES 格式为 "tc" 0x05 0x10 0x00 0x00
	if !(raw[0] == 116 && raw[1] == 99 && raw[2] == 5 && raw[3] == 16 && raw[4] == 0 && raw[5] == 0) {
		return nil, fmt.Errorf("unexpected header %v", raw[:6])
	}
	seed := raw[icubeHeaderLen : icubeHeaderLen+icubeSeedLen]
	key, iv := icubeDeriveKey(seed)
	ct := raw[icubeHeaderLen+icubeSeedLen:]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("bad ciphertext length %d", len(ct))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	pt, err = pkcs7Unpad(pt)
	if err != nil {
		return nil, err
	}
	if len(pt) <= icubeDigestLen {
		return nil, fmt.Errorf("plaintext too short (%d)", len(pt))
	}
	want := sha512.Sum512(pt[icubeDigestLen:])
	if !bytes.Equal(want[:], pt[:icubeDigestLen]) {
		return nil, fmt.Errorf("digest mismatch")
	}
	return pt[icubeDigestLen:], nil
}

// jwtDataID 从 JWT 的 payload 里取 data.id（Trae 会话 token 里即用户 uid）。
func jwtDataID(jwt string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		// 某些 token 用标准 base64 编码
		payload, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var p struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	return strings.TrimSpace(p.Data.ID)
}

// TraeCurrentUIDFromStorageRaw 从 storage.json 原文解析当前登录 uid。
// 依据：解密 `iCubeAuthInfo://icube.cloudide` → {"token": "<JWT>"} → JWT data.id。
// 解析失败返回空串（调用方自行回退）。
func TraeCurrentUIDFromStorageRaw(storageRaw []byte) (uid string) {
	_, uid = TraeClientJWTFromStorageRaw(storageRaw)
	return uid
}

// TraeClientJWTFromStorageRaw 从 Trae IDE 客户端的 storage.json 原文里解出
// 当前登录会话的 Cloud-IDE-JWT 与 uid。
//
// 客户端运行期间会自己刷新这份 token（JWT 有效期 8 小时），因此「客户端已登录」
// 的机器可以把它当作**免浏览器的令牌来源**：定时重读 + 落进签到/对话号池即可长期续命。
// 未登录 / 旧版格式 / 解密失败时返回空 jwt（不报错，调用方按「客户端不可用」处理）。
func TraeClientJWTFromStorageRaw(storageRaw []byte) (jwt, uid string) {
	defer func() { _ = recover() }()
	var m map[string]any
	if json.Unmarshal(storageRaw, &m) != nil {
		return "", ""
	}
	v, _ := m[traeCloudideKey].(string)
	if v == "" {
		return "", ""
	}
	plain, err := DecryptICubeBlob(v)
	if err != nil {
		return "", ""
	}
	var obj struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(plain, &obj) != nil || obj.Token == "" {
		return "", ""
	}
	return obj.Token, jwtDataID(obj.Token)
}
