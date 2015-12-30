package common

import (
	"crypto/rand"
	"log"
)

var (
	level int = 0
)

func FixHash(b []byte) []byte {
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
		rb[i] = (b[m] + b[m+1]) ^ byte(i*m) ^ rb[i-1]
		m = (m + 1) % (len(b) - 1)
	}

	return rb
}

func GenerateRandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	if err != nil {
		return nil, err
	}

	return b, nil
}

func SetLevel(l int) {
	level = l
}

func Info(format string, v ...interface{}) {
	if 1 <= level {
		log.Println(format, v)
	}
}

func Warn(format string, v ...interface{}) {
	if 0 <= level {
		log.Println(format, v)
	}
}
