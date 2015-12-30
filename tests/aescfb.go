package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"time"
	//"encoding/base64"
	"bytes"
	"fmt"
	"io"
)

func EncryptAESCFB(dst, src, key, iv []byte) error {
	aesBlockEncrypter, err := aes.NewCipher([]byte(key))
	if err != nil {
		return err
	}
	aesEncrypter := cipher.NewCFBEncrypter(aesBlockEncrypter, iv)
	fmt.Printf("dst len=%v %v\n", len(dst), len(src))
	aesEncrypter.XORKeyStream(dst[:8], src[:8])
	aesEncrypter.XORKeyStream(dst[8:], src[8:])
	return nil
}

func DecryptAESCFB(dst, src, key, iv []byte) error {
	aesBlockDecrypter, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil
	}
	aesDecrypter := cipher.NewCFBDecrypter(aesBlockDecrypter, iv)
	aesDecrypter.XORKeyStream(dst, src)
	return nil
}

// GenerateRandomBytes returns securely generated random bytes.
// It will return an error if the system's secure random
// number generator fails to function correctly, in which
// case the caller should not continue.
func GenerateRandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	// Note that err == nil only if we read len(b) bytes.
	if err != nil {
		return nil, err
	}

	return b, nil
}

func simpleFixHash(b []byte) []byte {
	l := 16
	rb := make([]byte, l)

	m := 0
	rb[0], rb[1] = b[0], b[1]

	n := l
	if n < len(b) {
		n = len(b)
	}

	for j := 1; j < n; j++ {
		i := j%(l-1) + 1
		//fmt.Println(i, j)
		rb[i] = (b[m] + b[m+1]) ^ byte(i*m) ^ rb[i-1]
		m = (m + 1) % (len(b) - 1)
	}

	return rb
}

func main() {
	const key10 = "1234567890"
	const key16 = "1234567890123486"
	const key24 = "123456789012348678901234"
	const key32 = "12345678901234567890123456789012"
	var key = key16
	var msg = "message888me88sagezzzmessage"
	//var iv = []byte(key)[:aes.BlockSize] // Using IV same as key is probably bad
	iv, _ := GenerateRandomBytes(aes.BlockSize)
	var err error

	//rb1 := simpleFixHash([]byte(key10))
	rb2 := simpleFixHash([]byte(key16))
	rb3 := simpleFixHash([]byte(key24))
	//fmt.Println("iv", aes.BlockSize)
	fmt.Println(rb2, rb3)
	key = string(rb2)

	// Encrypt
	encrypted := make([]byte, len(msg))
	err = EncryptAESCFB(encrypted, []byte(msg), []byte(key), iv)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Encrypting %v %s -> %v\n", []byte(msg), msg, encrypted)

	// Decrypt
	decrypted := make([]byte, len(msg))
	err = DecryptAESCFB(decrypted, encrypted, []byte(key), iv)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Decrypting %v -> %v %s\n", encrypted, decrypted, decrypted)

	block, _ := aes.NewCipher([]byte(key))
	encry := cipher.NewCFBEncrypter(block, iv)
	decry := cipher.NewCFBDecrypter(block, iv)

	en2 := make([]byte, len(msg))
	encry.XORKeyStream(en2, []byte(msg))

	en22 := make([]byte, len(msg))
	encry.XORKeyStream(en22, []byte(msg))

	en4 := make([]byte, len(msg))
	decry.XORKeyStream(en4, encrypted)
	fmt.Printf("decrypt: %v\n", string(en4))

	en3 := make([]byte, len(msg))
	decry.XORKeyStream(en3, en22)
	fmt.Printf("decrypt: %v\n", string(en3))

	//en3 := make([]byte, len(msg))
	//decry.XORKeyStream(en2, en22)
	//fmt.Printf("decrypt: %v\n", string(en3))

	//decry2 := cipher.NewCFBDecrypter(block, iv)
	//en5 := make([]byte, len(msg))
	//decry2.XORKeyStream(en5, encrypted)
	//fmt.Printf("decrypt: %v\n", string(en5))

	//en5 := make([]byte, len(msg))
	en5 := make([]byte, 0, len(msg))
	ben5 := bytes.NewBuffer(en5)
	rs := &cipher.StreamReader{S: cipher.NewCFBDecrypter(block, iv), R: bytes.NewBuffer(encrypted)}
	n, _ := io.Copy(ben5, rs)
	fmt.Printf("decrypt: n=%d %v\n", n, string(ben5.Bytes()))

	a1 := make([]byte, 0, 1024)
	b1 := bytes.NewBuffer(a1)
	fmt.Printf("len=%v %v\n", b1.Len(), cap(b1.Bytes()))
	b1_a := b1.Next(1024)
	fmt.Printf("len=%v\n", len(b1_a))

	a2 := make([]byte, 0, 1024)
	b2 := bytes.NewBuffer(a2)
	b2_2 := b2.Bytes()
	b2_3 := b2_2[:cap(b2_2)]
	b2_4 := bytes.NewBuffer(b2_3)
	b2.Grow(1024000)
	t1 := time.Now().UnixNano()
	b2.Write(b2_4.Bytes())
	t2 := time.Now().UnixNano()
	fmt.Printf("len=%v %v %v %v %v\n", b2.Len(), len(b2_2), len(b2_3), b2_4.Len(), (t2 - t1))
}
