package client

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/icholy/killable"

	common "../common"
)

type AssistConfig struct {
	serverAddr string
	localAddr  string
}

type AssistMgr struct {
	killable.Killable
	conf    *AssistConfig
	calls   chan *common.Future
	seqMax  int32
	tcpConn net.Conn
	udpConn *net.UDPConn
	addr    string
	hooks   map[int]*common.MsgHook
	in      chan *common.Msg
	out     chan *common.Msg
	hookAdd chan *common.MsgHook
	hookDel chan int
}

func (s *AssistMgr) Calls() chan *common.Future {
	return s.calls
}

func assistConfig() *AssistConfig {
	var saddr = flag.String("server", "ngrok.wang:18886", "server addr, like ngrok.wang:18886")
	flag.Parse()

	return &AssistConfig{
		serverAddr: *saddr,
		localAddr:  "0.0.0.0:18887",
	}
}

func newAssistMgr(conf *AssistConfig) *AssistMgr {
	return &AssistMgr{
		conf:     conf,
		Killable: killable.New(),
		calls:    make(chan *common.Future),
		hooks:    make(map[int]*common.MsgHook),
		in:       make(chan *common.Msg, common.MessageSeqSize),
		out:      make(chan *common.Msg, common.MessageSeqSize),
		hookAdd:  make(chan *common.MsgHook),
		hookDel:  make(chan int),
	}
}

func (ass *AssistMgr) Connect() error {
	var err error

	cert2_b, _ := Asset("client/cert2.pem")
	priv2_b, _ := Asset("client/cert2.key")
	priv2, _ := x509.ParsePKCS1PrivateKey(priv2_b)

	cert := tls.Certificate{
		Certificate: [][]byte{cert2_b},
		PrivateKey:  priv2,
	}

	config := tls.Config{Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true}
	ass.tcpConn, err = tls.Dial("tcp", ass.conf.serverAddr, &config)
	if err != nil {
		return err
	}
	return ass.Regist()
}

func (ass *AssistMgr) Regist() error {
	msg := common.NewMsg(0)
	defer msg.Free()

	msg.Hdr = common.MsgHdr{
		Type: common.MsgTypeAssistReg,
		Seq:  uint16(atomic.AddInt32(&ass.seqMax, 1)),
	}
	msg.Real = &common.MsgAssistReg{Addr: ass.conf.localAddr}
	ass.tcpConn.SetWriteDeadline(time.Now().Add(common.TcpTimeoutSec))
	if err := common.WriteTcpMsg(ass.tcpConn, msg.Dup()); err == nil {
		if rMsg, err2 := common.ReadTcpMsg(ass.tcpConn); err2 == nil {
			defer rMsg.Free()
			resp := rMsg.GetReal().(*common.MsgAssistReg)
			if resp != nil {
				log.Println("Assist regist ok", resp.Addr)
				ass.addr = resp.Addr
				return nil
			}
			return errors.New("ass empty response")
		} else {
			return err2
		}
	} else {
		return err
	}
}

func (ass *AssistMgr) udpInit() error {
	udpAddr, err := net.ResolveUDPAddr("udp", ass.conf.localAddr)
	if err != nil {
		return err
	}

	ass.udpConn, err = net.ListenUDP("udp", udpAddr)
	return err
}

func (ass *AssistMgr) tcpReadLoopFor() error {
	conn := ass.tcpConn
	conn.SetReadDeadline(time.Now().Add(common.TcpAssReadLoopSec))
	if msg, err := common.ReadTcpMsg(conn); err == nil {
		defer msg.Free()

		select {
		case <-ass.Dying():
			return common.ErrMsgKilled
		case ass.in <- msg.Dup():
			return nil
		}
	} else {
		log.Println("tcpRead error", err)
		ass.Kill(common.ErrMsgRead)
		return common.ErrMsgRead
	}
}

func (ass *AssistMgr) tcpWriteLoopFor() error {
	conn := ass.tcpConn
	select {
	case <-ass.Dying():
		return common.ErrMsgKilled
	case msg, ok := <-ass.out:
		if ok {
			defer msg.Free()
			conn.SetWriteDeadline((time.Now().Add(common.TcpAssWriteLoopSec)))
			if err := common.WriteTcpMsg(conn, msg.Dup()); err == nil {
				return nil
			} else {
				log.Println("write tcp error", err)
			}
		}
		//log.Println("out not ok", ass.out, msg)
	}

	ass.Kill(common.ErrMsgWrite)
	return common.ErrMsgWrite
}

func (ass *AssistMgr) readUdpLoopFor(uBuf []byte) error {
	n, addr, err := ass.udpConn.ReadFromUDP(uBuf)
	if nil == err {
		msg := common.NewMsg(0)
		defer msg.Free()
		log.Println("readUdp n=", n, addr)

		msg.V = addr
		if err2 := common.UnpackUdp(msg.Dup(), uBuf[:n]); err2 == nil {
			if msg.GetReal() != nil {
				//log.Println("Got message", msg.GetReal())
				/* switch msg.Hdr.Type {
				case common.MsgTypeUdpPing:
					msg.Hdr.Type = common.MsgTypeUdpPong
					msg.GetReal().(*common.MsgUdpPong).Addr = fmt.Sprintf("%v", addr)
					common.WriteUdpMsgServer(msg.Dup(), addr, ass.udpConn)

				case common.MsgTypeUdpPong:

				default:
					select {
					case <-ass.Dying():
					case ass.in <- msg.Dup():
					}
				} */
				select {
				case <-ass.Dying():
				case ass.in <- msg.Dup():
				}
			} else {
				log.Println("ignore empty message", msg.Hdr.Type)
			}

			return nil
		} else {
			err = err2
		}
	}

	ass.Kill(common.ErrMsgWrite)
	return err
}

func (ass *AssistMgr) tcpWriteLoop() {
	for {
		if err := ass.tcpWriteLoopFor(); err != nil {
			log.Println("tcpWriteLoop error:", err)
			return
		}
	}
}

func (ass *AssistMgr) tcpReadLoop() {
	for {
		if err := ass.tcpReadLoopFor(); err != nil {
			log.Println("tcpReadLoop error:", err)
			return
		}
	}
}

func (ass *AssistMgr) udpReadLoop() {
	buf := make([]byte, 1500)
	for {
		if err := ass.readUdpLoopFor(buf); err != nil {
			log.Println("readUdpLoopFor error:", err)
			return
		}
	}
}

func (ass *AssistMgr) runFor(tick *time.Ticker) error {
	ptask := ass
	select {
	case <-ass.Dying():
		//log.Println("dying")
		//Always close all connections
		//Can only killed
		ass.GoDying()
		return killable.ErrDying
	case future, ok := <-ptask.Calls():
		if ok {
			future.Resp <- future.F(future, future.Arg)
		}
	case msg, ok := <-ass.in:
		if ok {
			//Not msg.Dup() hear
			ass.handleIncomeMsg(msg)
		}
	case <-tick.C:
		ass.tcpPingServ()
	}

	return nil
}

func (ass *AssistMgr) handleIncomeMsg(msg *common.Msg) {
	//log.Println("got msg", msg)
	defer msg.Free()

	switch msg.Hdr.Type {
	case common.MsgTypeUdpPing:
		msgPing := msg.GetReal().(*common.MsgUdpPong)
		msgPing.Addr = fmt.Sprintf("%v", msg.V)
		ass.out <- msg.Dup()
	case common.MsgTypeP2pTest:
		msgPing := msg.GetReal().(*common.MsgUdpPong)
		go func(m *common.Msg) {
			defer m.Free()

			if udpAddr, err := net.ResolveUDPAddr("udp", msgPing.Addr); err != nil {
				log.Println("error for resolve udp", msgPing.Addr, err)
				return
			} else {
				//Just mark as different
				oldSeq := m.Hdr.Seq
				m.Hdr.Seq += 10000
				for i := 0; i < 2; i++ {
					common.WriteUdpMsgServer(m.Dup(), udpAddr, ass.udpConn)
					time.Sleep(20 * time.Millisecond)
				}
				//restore back to old seq
				m.Hdr.Seq = oldSeq
			}
			//log.Println("response to server")
			//TODO what if ass.out is closed?
			ass.out <- m.Dup()
		}(msg.Dup())

	case common.MsgTypeAssPong:
	}
}

func (ass *AssistMgr) tcpPingServ() {
	msg := common.NewMsg(0)
	defer msg.Free()

	msg.Real = &common.MsgTcpPong{}
	msg.Hdr = common.MsgHdr{
		Type: common.MsgTypeAssPing,
		Seq:  uint16(atomic.AddInt32(&ass.seqMax, 1)),
	}
	ass.out <- msg.Dup()
}

func (ass *AssistMgr) Run() error {
	tick := time.NewTicker(common.TcpAssPingSec)
	defer tick.Stop()

	for {
		if err := ass.runFor(tick); err != nil {
			return err
		}
	}
}

func (ass *AssistMgr) GoDying() {
	ass.tcpConn.Close()
	ass.udpConn.Close()
}

func startAssist(conf *AssistConfig) error {
	var err error
	ass := newAssistMgr(conf)
	if err := ass.Connect(); err != nil {
		return err
	}

	if err = ass.udpInit(); err != nil {
		return err
	}

	go ass.tcpReadLoop()
	go ass.tcpWriteLoop()
	go ass.udpReadLoop()

	defer func() {
		close(ass.in)
		close(ass.out)
	}()

	killable.Do(ass, func() error {
		return ass.Run()
	})

	<-ass.Dead()

	return common.ErrMsgKilled
}

func AssistMain() {
	common.MsgPoolInit()
	conf := assistConfig()

	for {
		err := startAssist(conf)
		log.Println("wait for 20s for reconnect", err)
		time.Sleep(20 * time.Second)
	}
}
