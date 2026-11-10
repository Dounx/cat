package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
)

// codec implements WeCom's callback encryption: AES-256-CBC with the key
// doubling as IV source, PKCS#7 padding, and a SHA-1 signature over the
// sorted token, timestamp, nonce and ciphertext.
type codec struct {
	token  string
	key    []byte
	corpID string
}

func newCodec(token, encodingAESKey, corpID string) (*codec, error) {
	key, err := base64.StdEncoding.DecodeString(encodingAESKey + "=")
	if err != nil || len(key) != 32 {
		return nil, errors.New("EncodingAESKey 无效（应为 43 个字符）")
	}
	return &codec{token: token, key: key, corpID: corpID}, nil
}

func (c *codec) signature(timestamp, nonce, encrypted string) string {
	parts := []string{c.token, timestamp, nonce, encrypted}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}

func (c *codec) verify(sig, timestamp, nonce, encrypted string) bool {
	return subtle.ConstantTimeCompare([]byte(sig), []byte(c.signature(timestamp, nonce, encrypted))) == 1
}

// decrypt returns the message inside a base64 ciphertext, checking that it
// was addressed to this corp.
func (c *codec) decrypt(encrypted string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil || len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("密文格式错误")
	}
	block, _ := aes.NewCipher(c.key)
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, c.key[:aes.BlockSize]).CryptBlocks(plain, data)
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > 32 || pad > len(plain) {
		return nil, errors.New("解密失败（EncodingAESKey 不匹配？）")
	}
	plain = plain[:len(plain)-pad]
	if len(plain) < 20 {
		return nil, errors.New("解密失败")
	}
	n := int(binary.BigEndian.Uint32(plain[16:20]))
	if n < 0 || 20+n > len(plain) {
		return nil, errors.New("解密失败")
	}
	if receiver := string(plain[20+n:]); receiver != c.corpID {
		return nil, errors.New("消息不是发给这个企业的")
	}
	return plain[20 : 20+n], nil
}

// encrypt is the inverse of decrypt; random must be 16 bytes.
func (c *codec) encrypt(msg, random []byte) string {
	var buf bytes.Buffer
	buf.Write(random[:16])
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(msg)))
	buf.Write(msg)
	buf.WriteString(c.corpID)
	pad := 32 - buf.Len()%32
	buf.Write(bytes.Repeat([]byte{byte(pad)}, pad))
	block, _ := aes.NewCipher(c.key)
	out := make([]byte, buf.Len())
	cipher.NewCBCEncrypter(block, c.key[:aes.BlockSize]).CryptBlocks(out, buf.Bytes())
	return base64.StdEncoding.EncodeToString(out)
}
