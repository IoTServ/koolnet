package common

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

const (
	MsgTypeReq        = 1
	MsgTypeUdpPing    = 2
	MsgTypeUdpPong    = 3
	MsgTypeAddrReq    = 4
	MsgTypeAddrResp   = 5
	MsgTypeHandshake  = 6
	MsgTypeP2pPing    = 7
	MsgTypeP2pPong    = 8
	MsgTypeRegResp    = 9
	MsgTypeP2pTest    = 10
	MsgTypeIPChange   = 11
	MsgTypeUdpValid   = 12
	MsgTypeDoShake    = 13
	MsgTypeTcpPing    = 14
	MsgTypeTcpPong    = 15
	MsgTypeAssistReg  = 16
	MsgTypeAssistResp = 17
	MsgTypeAssPing    = 18
	MsgTypeAssPong    = 19
	MsgTypeClientQuit = 20

	MsgTypeIkcp   = 100
	MsgTypeSyn    = 101
	MsgTypeSynOk  = 102
	MsgTypeSynErr = 103
	MsgTypeFin    = 104
	MsgTypeData   = 105

	MsgHdrSize     = 8
	KindListen     = "listen"
	MessageSeqSize = 128
	MsgSizeSmall   = 1024
	MsgSizeBig     = 2600
	EncryptSize    = 16
	MsgCtlSize     = 16
	MsgXor         = 0x9C

	UdpPongTypeTwoServer = 0
	UdpPongTypeFromAss   = 1
	UdpPongTypeOneServer = 3

	TcpTickCount          = 20
	TcpTimeoutSecCount    = 3 * TcpTickCount
	TcpTimeoutSec         = time.Second * TcpTimeoutSecCount
	TcpAssPingSec         = time.Second * TcpTickCount
	TcpAssReadLoopSec     = TcpTimeoutSec
	TcpAssWriteLoopSec    = time.Second * TcpTickCount
	TcpClientPingSec      = time.Second * TcpTickCount
	TcpClientReadLoopSec  = TcpTimeoutSec
	TcpClientWriteLoopSec = time.Second * TcpTickCount
	UdpP2pPingTick        = 5
	UdpP2pPingSec         = time.Second * UdpP2pPingTick
	UdpP2pPingTimeout     = time.Second * UdpP2pPingTick * 2
	UdpP2pPingClose       = UdpP2pPingTimeout * 3

	AssPass = ""
)

var (
	ErrMsgRegExists    = errors.New("message: registNameExists")
	ErrMsgRegNotFound  = errors.New("message: registNameNotFound")
	ErrMsgWrite        = errors.New("message: writeMessageError")
	ErrMsgRead         = errors.New("message: readMessageError")
	ErrMsgSpaceExit    = errors.New("message: spaceExit")
	ErrMsgKilled       = errors.New("message: killedByOther")
	ErrMsgRealEmpty    = errors.New("message: empty real")
	ErrMsgNotSupport   = errors.New("message: notSupport")
	ErrMsgNone         = errors.New("message: none")
	ErrMsgShakeTimeout = errors.New("message: shakeTimeout")
	ErrMsgPanic        = errors.New("message: Panic")
)

type MsgPool struct {
	alloc      int32
	free       int32
	bpools     []sync.Pool
	mpool      sync.Pool
	msgBufPool sync.Pool
}

type MsgHdr struct {
	Type uint8
	Addr uint8
	Port uint16
	Len  uint16
	Seq  uint16
}

//FIXME: origin CANOT makeSlice
type Msg struct {
	Hdr    MsgHdr
	origin *bytes.Buffer
	Real   interface{}
	V      interface{}
	refcnt int32

	//For log
	//Id int32
}

type MsgBuf struct {
	buf    []byte
	Start  int
	Size   int
	Type   int
	refcnt int32

	//For log
	//Id int32
}

var msgPool *MsgPool

func MsgPoolInit() {
	msgPool = &MsgPool{bpools: make([]sync.Pool, 2),
		mpool: sync.Pool{
			New: func() interface{} {
				return new(Msg)
			},
		},
	}
	msgPool.bpools[0] = sync.Pool{
		New: func() interface{} {
			return bytes.NewBuffer(make([]byte, 0, MsgSizeSmall))
		},
	}
	msgPool.bpools[1] = sync.Pool{
		New: func() interface{} {
			return bytes.NewBuffer(make([]byte, 0, MsgSizeBig))
		},
	}
	msgPool.msgBufPool = sync.Pool{
		New: func() interface{} {
			return &MsgBuf{
				buf:    make([]byte, MsgSizeBig),
				Size:   0,
				refcnt: 0,
			}
		},
	}
}

func NewMsg(bsize int) *Msg {
	msg := msgPool.mpool.Get().(*Msg)
	msg.ResetBuffer(bsize)
	msg.refcnt = 1

	//For log
	//cnt := atomic.AddInt32(&msgPool.alloc, 1)
	//msg.Id = cnt
	//log.Println("alloc", cnt)

	return msg
}

// Ignore the prefix buffer and request a new.
// Caution for this func, should only use one time at first !!!
func (m *Msg) ResetBuffer(bsize int) {
	if m.origin != nil {
		//Reset first, drop the old
		m.origin.Reset()
		if m.origin.Cap() >= bsize || m.origin.Cap() >= MsgSizeBig {
			//Don't need to reset
			return
		}
		if m.origin.Cap() > MsgSizeSmall {
			msgPool.bpools[1].Put(m.origin)
		} else {
			msgPool.bpools[0].Put(m.origin)
		}
	}
	if bsize > MsgSizeSmall {
		m.origin = msgPool.bpools[1].Get().(*bytes.Buffer)
	} else {
		m.origin = msgPool.bpools[0].Get().(*bytes.Buffer)
	}
}

func (m *Msg) Free() {
	if v := atomic.AddInt32(&m.refcnt, -1); v > 0 {
		//log.Println("has ", v)
		return
	}

	//cnt := atomic.AddInt32(&msgPool.free, 1)
	//log.Println("freeing", m.Id, "count", cnt)

	m.origin.Reset()
	if m.origin.Cap() > MsgSizeSmall {
		msgPool.bpools[1].Put(m.origin)
	} else {
		msgPool.bpools[0].Put(m.origin)
	}
	m.origin = nil
	m.Real = nil
	m.V = nil
	msgPool.mpool.Put(m)
}

func (m *Msg) Dup() *Msg {
	atomic.AddInt32(&m.refcnt, 1)
	return m
}

func (m *Msg) GetOrigin() *bytes.Buffer {
	return m.origin
}

//TODO what if nil
func (m *Msg) GetReal() interface{} {
	//Just got from old
	if nil != m.Real {
		//log.Println(m.Real)
		return m.Real
	}
	if m.Hdr.Len > 0 && m.origin.Len() != 0 {
		var r interface{}
		switch m.Hdr.Type {
		case MsgTypeReq:
			r = &MsgReg{}
		case MsgTypeUdpPong, MsgTypeUdpPing, MsgTypeP2pTest:
			r = &MsgUdpPong{}
		case MsgTypeAddrReq, MsgTypeAddrResp, MsgTypeIPChange, MsgTypeDoShake:
			r = &MsgReqAddr{}
		case MsgTypeHandshake:
			r = &MsgHandshake{}
		case MsgTypeRegResp:
			r = &MsgRegResp{}
		case MsgTypeTcpPing, MsgTypeTcpPong, MsgTypeAssPing, MsgTypeAssPong:
			r = &MsgTcpPong{}
		case MsgTypeAssistReg, MsgTypeAssistResp:
			r = &MsgAssistReg{}
		case MsgTypeP2pPong, MsgTypeP2pPing:
			r = &MsgP2pPong{}
		case MsgTypeClientQuit:
			r = &MsgClientQuit{}
		default:
			Warn("mesasge type not found typ=", m.Hdr.Type)
			return nil
		}

		//Always be bytes
		//log.Println("origin bytes:", m.Hdr.Type, string(m.origin.Bytes()))
		if err := json.Unmarshal(m.origin.Bytes(), r); err == nil {
			m.origin.Reset()
			m.Real = r
			return m.Real
		} else {
			Warn("message unmarshal error", err)
		}
	}

	return nil
}

func NewMsgBuf() *MsgBuf {
	b := msgPool.msgBufPool.Get().(*MsgBuf)
	b.refcnt = 1

	//cnt := atomic.AddInt32(&msgPool.alloc, 1)
	//b.Id = cnt
	//log.Println("alloc buf", cnt)

	return b
}

func (mb *MsgBuf) Dup() *MsgBuf {
	atomic.AddInt32(&mb.refcnt, 1)
	return mb
}

func (mb *MsgBuf) Free() {
	if v := atomic.AddInt32(&mb.refcnt, -1); v > 0 {
		//log.Println("has ", v)
		return
	}

	//cnt := atomic.AddInt32(&msgPool.free, 1)
	//log.Println("freeing", mb.Id, "count", cnt)

	mb.Size = 0
	mb.Start = 0
	mb.Type = 0
	msgPool.msgBufPool.Put(mb)
}

func (mb *MsgBuf) GetBuf() []byte {
	return mb.buf
}

func (mb *MsgBuf) GetReal() []byte {
	return mb.buf[mb.Start:(mb.Start + mb.Size)]
}

type TunnelItem struct {
	ProxyAddr string
	LocalAddr string
}

type MsgReg struct {
	Name  string
	Pass  string
	Kind  string
	Items []TunnelItem
	Iv    string
}

type MsgRegResp struct {
	ClientId int
	IP       string
	Items    []TunnelItem
	Iv       string
	Status   string
}

type MsgTcpPong struct {
}

/* type MsgUdpPing struct {
	Space      string
	ClientId   int
	LocalAddrs []string
	Addr       string
} */

type MsgUdpPong struct {
	Type     int
	Space    string
	ClientId int
	Addr     string
	Alter    string
	Random   int
}

//NewHandshake, send from tcp channel
type MsgReqAddr struct {
	From      int
	To        int
	FromPub   string
	FromPub2  string
	FromAddrs []string
	ToPub     string
	ToPub2    string
	Addrs     []string
}

type MsgHandshake struct {
	//Sendto addr, only choose by request side
	RAddr  string
	Choose string
	Cands  []string
}

type MsgP2pPing struct {
	From int
}

type MsgP2pPong struct {
	From int
}

type MsgAssistReg struct {
	Addr string
	Pass string
}

type MsgClientQuit struct {
	ClientId int
}
