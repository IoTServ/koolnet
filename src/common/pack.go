package common

import (
	"bytes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
)

func ReadTcpMsg(c net.Conn) (*Msg, error) {
	msg := NewMsg(0)
	defer msg.Free()

	err := binary.Read(c, binary.BigEndian, &msg.Hdr)
	if nil != err {
		return nil, err
	}

	if int(msg.Hdr.Len) > msg.GetOrigin().Cap() {
		return nil, ErrMsgSpaceExit
	}

	msg.ResetBuffer(int(msg.Hdr.Len))
	n, err := io.CopyN(msg.GetOrigin(), c, int64(msg.Hdr.Len))
	if nil != err {
		return nil, err
	}

	if int(n) != int(msg.Hdr.Len) {
		err = errors.New(fmt.Sprintf("Expected to read %v bytes, but only read %d", msg.Hdr.Len, n))
		return nil, err
	}

	if msg.GetReal() == nil {
		err = errors.New(fmt.Sprintf("Ignore error message type:%v", msg.Hdr.Type))
		return nil, err
	}

	//Add refcnt
	return msg.Dup(), nil
}

func WriteTcpMsg(c net.Conn, m *Msg) error {
	//Always free it
	defer m.Free()

	b1, err := json.Marshal(m.Real)
	if nil == err {
		m.Hdr.Len = uint16(len(b1))
		if err = binary.Write(c, binary.BigEndian, m.Hdr); err == nil {
			if _, err = c.Write(b1); err == nil {
				return nil
			}
		}
	}

	return err
}

func WriteTcpReal(seq uint16, typ uint8, from uint8, lport uint16, o interface{}, c net.Conn) error {
	m := NewMsg(0)
	defer m.Free()

	m.Hdr = MsgHdr{
		Seq:  seq,
		Type: typ,
		Addr: from,
		Port: lport,
	}
	m.Real = o
	return WriteTcpMsg(c, m.Dup())
}

func UnpackUdp(msg *Msg, uBuf []byte) error {
	defer msg.Free()

	bf := bytes.NewBuffer(uBuf[:MsgHdrSize])
	err := binary.Read(bf, binary.BigEndian, &msg.Hdr)
	if nil == err {
		xor := uBuf[MsgHdrSize:]
		for i := 0; i < len(xor); i++ {
			xor[i] = xor[i] ^ MsgXor
		}
		msg.GetOrigin().Write(xor)
		return nil
	}

	return err
}

//https://golang.org/src/crypto/cipher/example_test.go
//http://studygolang.com/articles/1829
func UnpackUdpMsg(block cipher.Block, iv []byte, msg *Msg, rBuf []byte) error {
	defer msg.Free()
	var err error

	//First 8 Bytes is not encrypted
	msg.GetOrigin().Write(rBuf[:MsgHdrSize])
	if rBuf[0] != MsgTypeP2pTest {
		rs := &cipher.StreamReader{S: cipher.NewCFBDecrypter(block, iv), R: bytes.NewBuffer(rBuf[MsgHdrSize:])}
		if _, err = io.Copy(msg.GetOrigin(), rs); err != nil {
			return err
		}
	} else {
		xor := rBuf[MsgHdrSize:]
		for i := 0; i < len(xor); i++ {
			xor[i] = xor[i] ^ MsgXor
		}
		msg.GetOrigin().Write(xor)
	}

	if msg.GetOrigin().Bytes()[0] == MsgTypeIkcp {
		//IKCP data, set Hdr manual
		msg.Hdr = MsgHdr{
			Type: MsgTypeIkcp,
			//(c.clientId << 16)
			Addr: msg.GetOrigin().Bytes()[2],
			Len:  uint16(len(rBuf)),
		}
	} else {
		//Normal data, Read the header first
		if err = binary.Read(msg.GetOrigin(), binary.BigEndian, &msg.Hdr); err == nil {
			return nil
		}
	}

	return err
}

func WriteUdpMsg(block cipher.Block, iv []byte, m *Msg, dest *net.UDPAddr, udpConn *net.UDPConn) error {
	defer m.Free()

	mb := NewMsgBuf()
	defer mb.Free()

	if m.Real == nil {
		Warn("m.Real is nil")
		return ErrMsgNone
	}

	b1, err := json.Marshal(m.Real)
	if nil == err {
		s := cipher.NewCFBEncrypter(block, iv)

		m.Hdr.Len = uint16(len(b1))
		mb.Size = len(b1) + 8
		bf := bytes.NewBuffer([]byte{})
		binary.Write(bf, binary.BigEndian, &m.Hdr)

		//Not encrypt first 8 Bytes
		copy(mb.GetReal()[:MsgHdrSize], bf.Bytes()[:MsgHdrSize])
		//s.XORKeyStream(mb.GetReal()[MsgUnencryptLen:MsgHdrSize], bf.Bytes()[MsgUnencryptLen:])

		s.XORKeyStream(mb.GetReal()[MsgHdrSize:], b1)
		udpConn.WriteToUDP(mb.GetReal(), dest)
		//log.Println("Real is, n=", len(mb.GetReal()), mb.GetReal())
	}
	return err
}

func WriteUdpMsgServer(m *Msg, addr *net.UDPAddr, udpConn *net.UDPConn) error {
	defer m.Free()

	b1, err := json.Marshal(m.Real)
	if nil == err {
		n := len(b1)
		m.Hdr.Len = uint16(n)
		binary.Write(m.GetOrigin(), binary.BigEndian, &m.Hdr)

		for i := 0; i < n; i++ {
			b1[i] = b1[i] ^ MsgXor
		}
		m.GetOrigin().Write(b1)

		udpConn.WriteToUDP(m.GetOrigin().Bytes(), addr)
	}
	return err
}

/* func WriteUdpConnBuf(block cipher.Block, iv []byte, hdr []byte, data []byte, dest *net.UDPAddr, udpConn *net.UDPConn) error {
	mb := NewMsgBuf()
	defer mb.Free()

	s := cipher.NewCFBEncrypter(block, iv)
	mb.Size = len(hdr) + len(data)

	s.XORKeyStream(mb.GetReal()[0:len(hdr)], hdr)
	s.XORKeyStream(mb.GetReal()[len(hdr):], data)
	_, err := udpConn.WriteToUDP(mb.GetReal(), dest)
	//log.Println("mb size is", mb.Size, data, mb.GetReal())
	return err
} */

//write ikcp data
func WriteUdpConnOnly(block cipher.Block, iv []byte, data []byte, dest *net.UDPAddr, udpConn *net.UDPConn) error {
	mb := NewMsgBuf()
	defer mb.Free()

	s := cipher.NewCFBEncrypter(block, iv)
	mb.Size = len(data)

	//Not encrypt first 8 Bytes
	copy(mb.GetReal()[:MsgHdrSize], data[:MsgHdrSize])

	s.XORKeyStream(mb.GetReal()[MsgHdrSize:], data[MsgHdrSize:])
	_, err := udpConn.WriteToUDP(mb.GetReal(), dest)
	return err
}
