package bot

import (
	"bytes"
	"crypto/aes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// CDN 媒体加解密。
//
// ## 参数全部来自真实数据实测
//
// （2026-09-14，真实图片消息，288784 字节 JPEG）
//
//   - 算法：`aes-128-ecb`
//   - 密钥：客户端拿到的 `aeskey` 是 **32 个十六进制字符**，按 hex 解码成 16 字节
//   - 填充：**标准 PKCS#7**
//
// ## 关于填充的教训
//
// 最初误判为「服务端自带填充、需关掉自动填充」，理由是密文 288800 比
// `mid_size` 288784 多 16 字节。实测尾部是 `1010…10`（16 个 0x10），正是 PKCS#7；
// 剥掉后长度恰好回到 288784。**多出的 16 字节就是填充本身，不是「已填充的内容」。**
//
// 技术方案原文只有一句「AES-128-ECB 加解密」，密钥来源、编码、填充方式全无。
// 以上均靠真实数据试出。
//
// ## 为什么自己按块加密而不用 cipher.NewCBCEncrypter
//
// **零 IV 的 CBC 不是 ECB。** CBC 会把前一块的密文异或进后一块，于是依赖关系
// 串起来；ECB 才是每块独立。协议要的是 ECB，所以只能自己按块走——
// 用一个「IV 全零的 CBC」去凑，看着能跑通解密（因为每块都独立解），但**加密方向
// 产生的密文是错的**，上传会失败，而错误信息只会说「服务端拒绝」。
const aesKeyBytes = 16

// aesBlockBytes AES 的分组长度，ECB 每块的大小。
const aesBlockBytes = 16

// ErrEmptyCiphertext 密文为空。
var ErrEmptyCiphertext = errors.New("密文为空，无可解密内容")

// ParseAesKey 把协议里的 `aeskey` 字符串转成密钥字节。
//
// 实测格式是 32 个十六进制字符。同时兼容 base64 形式（真实数据里
// `media.aes_key` 就是 base64，解码后与 hex 形式一致）。
func ParseAesKey(aeskey string) ([]byte, error) {
	if aeskey == "" {
		return nil, errors.New("aeskey 为空，无法解密")
	}

	if len(aeskey) == 2*aesKeyBytes {
		if decoded, err := hex.DecodeString(aeskey); err == nil {
			return decoded, nil
		}
	}

	// base64：可能是 16 字节原始密钥，也可能是 base64 包裹的 hex 字符串
	fromB64, err := base64.StdEncoding.DecodeString(aeskey)
	if err != nil {
		return nil, fmt.Errorf("无法识别的 aeskey 格式（长度 %d）", len(aeskey))
	}
	if len(fromB64) == aesKeyBytes {
		return fromB64, nil
	}
	if len(fromB64) == 2*aesKeyBytes {
		if decoded, hexErr := hex.DecodeString(string(fromB64)); hexErr == nil {
			return decoded, nil
		}
	}

	return nil, fmt.Errorf("无法识别的 aeskey 格式（长度 %d）", len(aeskey))
}

// assertKey 校验密钥长度，给出明确错误而不是让 aes.NewCipher 抛晦涩信息。
func assertKey(key []byte) error {
	if len(key) != aesKeyBytes {
		return fmt.Errorf("密钥长度必须是 %d 字节，实际 %d 字节", aesKeyBytes, len(key))
	}
	return nil
}

// DecryptMedia 解密 CDN 下载到的内容。标准 PKCS#7 剥填充。
func DecryptMedia(encrypted []byte, aeskey string) ([]byte, error) {
	if len(encrypted) == 0 {
		return nil, ErrEmptyCiphertext
	}
	if len(encrypted)%aesBlockBytes != 0 {
		return nil, fmt.Errorf("密文长度 %d 不是 %d 的整数倍，不是有效的 AES 数据",
			len(encrypted), aesBlockBytes)
	}

	key, err := ParseAesKey(aeskey)
	if err != nil {
		return nil, err
	}
	if err := assertKey(key); err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	// ECB：每块独立解密，块间**不**传递任何东西
	plain := make([]byte, len(encrypted))
	for offset := 0; offset < len(encrypted); offset += aesBlockBytes {
		block.Decrypt(plain[offset:offset+aesBlockBytes], encrypted[offset:offset+aesBlockBytes])
	}

	return stripPKCS7(plain)
}

// EncryptMedia 加密内容以便上传 CDN。与 DecryptMedia 对称。
//
// ⚠️ **本函数尚未经真实数据验证**——上传方向还没做过真机测试。实现按解密侧
// 实测到的规则镜像而来，届时需用真实上传流程复核。
func EncryptMedia(plain []byte, aeskey string) ([]byte, error) {
	key, err := ParseAesKey(aeskey)
	if err != nil {
		return nil, err
	}
	if err := assertKey(key); err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	// 先补 PKCS#7，再按块独立加密
	padded, err := addPKCS7(plain)
	if err != nil {
		return nil, err
	}

	out := make([]byte, len(padded))
	for offset := 0; offset < len(padded); offset += aesBlockBytes {
		block.Encrypt(out[offset:offset+aesBlockBytes], padded[offset:offset+aesBlockBytes])
	}
	return out, nil
}

// pkcs7PadSize PKCS#7 补多少字节。
//
// **明文正好是整块时也要补一整块**（补 16 个 0x10），不是 0 字节——
// 否则解出来的明文与原文同长，调用方无法区分「没填充」与「刚好整块」。
func pkcs7PadSize(length int) int {
	remainder := length % aesBlockBytes
	if remainder == 0 {
		return aesBlockBytes
	}
	return aesBlockBytes - remainder
}

func addPKCS7(plain []byte) ([]byte, error) {
	padding := pkcs7PadSize(len(plain))
	if padding == 0 {
		return nil, errors.New("PKCS#7 填充长度算出了 0，这不该发生")
	}
	out := make([]byte, 0, len(plain)+padding)
	out = append(out, plain...)
	return append(out, bytes.Repeat([]byte{byte(padding)}, padding)...), nil
}

// stripPKCS7 剥掉 PKCS#7 填充。
//
// **填充字节的值必须逐个核对**，不能只看最后一个：那样的话，一个被篡改或
// 随机损坏的密文有 1/256 的概率通过校验，然后解出**尾部是垃圾的明文**。
// 而这里解出来的正是图片——一个尾部糊掉的 JPEG 会渲染成半张图，且不报错。
func stripPKCS7(plain []byte) ([]byte, error) {
	if len(plain) == 0 || len(plain)%aesBlockBytes != 0 {
		return nil, fmt.Errorf("明文长度 %d 不是 %d 的整数倍", len(plain), aesBlockBytes)
	}

	padding := int(plain[len(plain)-1])
	if padding == 0 || padding > aesBlockBytes || padding > len(plain) {
		return nil, fmt.Errorf("PKCS#7 填充长度非法：%d", padding)
	}
	for _, b := range plain[len(plain)-padding:] {
		if int(b) != padding {
			return nil, errors.New("PKCS#7 填充字节不一致，密文可能被篡改或损坏")
		}
	}
	return plain[:len(plain)-padding], nil
}
