package server

import (
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/icholy/killable"

	common "../common"
)

type ServerMgr struct {
	killable.Killable
	calls      chan *common.Future
	serverAddr string
	seqMax     int32
	tcpConn    net.Listener
	udpConn    *net.UDPConn
	spaces     map[string]*Space
	assists    map[string]*Assist
	in         chan *common.Msg

	hooks   map[int]*common.MsgHook
	hookAdd chan *common.MsgHook
	hookDel chan int
}

func (s *ServerMgr) HookAdd() chan *common.MsgHook {
	return s.hookAdd
}

func (s *ServerMgr) HookDel() chan int {
	return s.hookDel
}

func (s *ServerMgr) Calls() chan *common.Future {
	return s.calls
}

type Space struct {
	spaceProp *SpaceProp
	endpoints map[int]*Endpoint
}

type SpaceProp struct {
	name  string
	pass  string
	iv    string
	items []common.TunnelItem
}

func (space *Space) unique() int {
	rand.Seed(time.Now().Unix())

	for i := 0; i < 10; i++ {
		id := rand.Intn(0xFD) + 1
		if _, ok := space.endpoints[id]; !ok {
			return id
		}
	}

	return 0
}

var serverMgr *ServerMgr

func getMgr() *ServerMgr {
	return serverMgr
}

func serverInit() *ServerMgr {

	var saddr = flag.String("addr", "0.0.0.0:18886", "server addr, like 0.0.0.0:18886")
	flag.Usage = func() {
		fmt.Printf("Usage: p2pserver -addr=0.0.0.0:18888\n\nBy Xiaobao, contact me at http://koolshare.cn\n\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	//First of all, init buffer
	common.MsgPoolInit()

	return &ServerMgr{
		serverAddr: *saddr,
		calls:      make(chan *common.Future),
		spaces:     make(map[string]*Space),
		assists:    make(map[string]*Assist),
		in:         make(chan *common.Msg, common.MessageSeqSize),

		hooks:    make(map[int]*common.MsgHook),
		hookAdd:  make(chan *common.MsgHook),
		hookDel:  make(chan int),
		Killable: killable.New(),
	}
}

func (s *ServerMgr) listenerInit() error {
	ca_b, _ := Asset("server/ca.pem")
	ca, _ := x509.ParseCertificate(ca_b)
	priv_b, _ := Asset("server/ca.key")
	priv, _ := x509.ParsePKCS1PrivateKey(priv_b)

	pool := x509.NewCertPool()
	pool.AddCert(ca)

	cert := tls.Certificate{
		Certificate: [][]byte{ca_b},
		PrivateKey:  priv,
	}

	config := tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
	}
	config.Rand = crand.Reader
	var err error
	s.tcpConn, err = tls.Listen("tcp", s.serverAddr, &config)
	return err
}

func (s *ServerMgr) udpInit() error {
	udpAddr, err := net.ResolveUDPAddr("udp", serverMgr.serverAddr)
	if err != nil {
		return err
	}
	serverMgr.udpConn, err = net.ListenUDP("udp", udpAddr)
	return err
}

func (s *ServerMgr) handleTcpConn() {
	for {
		select {
		case <-s.Dying():
			return
		default:
			if conn, err := s.tcpConn.Accept(); err == nil {
				go s.handleTcpClient(conn)
			} else {
				log.Println("tcpConn accept", err)
				s.Kill(common.ErrMsgKilled)
			}
		}
	}
}

func (s *ServerMgr) handleTcpClient(conn net.Conn) {
	defer conn.Close()
	o, err := s.registTcpClient(conn)
	if err != nil {
		log.Println("create endpoint error", err)
		return
	}

	switch v := o.(type) {
	case *Endpoint:
		endpoint := v
		defer endpoint.Close(s)
		go endpoint.WriteLoop(s)
		go endpoint.ReadLoop(s)
		killable.Do(endpoint, func() error {
			return endpoint.RunS(s)
		})

	case *Assist:
		ass := v
		log.Println("created", ass.localAddr)
		go ass.WriteLoop(s)
		ass.ReadLoop(s)
		//log.Println("closed ass?")
	default:
		log.Println("regist fail")
	}
}

func (s *ServerMgr) registSpace(conn net.Conn, msg *common.Msg) (*Endpoint, error) {
	defer msg.Free()

	regMsg := msg.GetReal().(*common.MsgReg)
	endpoint := newEndpoint(regMsg, conn, s)
	p1 := common.NewPromise(s).Then(func(pt common.PromiseTask, arg interface{}) (
		common.PromiseTask, interface{}, error) {
		s := pt.(*ServerMgr)
		end := arg.(*Endpoint)
		if end.clientId == 0 {
			if _, ok := s.spaces[end.space]; ok {
				return nil, nil, common.ErrMsgRegExists
			} else {
				space := &Space{spaceProp: end.spaceProp, endpoints: make(map[int]*Endpoint)}
				space.endpoints[end.clientId] = end
				s.spaces[end.space] = space
			}
		} else {
			if space, ok := s.spaces[end.space]; ok && space.spaceProp.pass == regMsg.Pass {
				end.clientId = space.unique()
				end.spaceProp = space.spaceProp
				space.endpoints[end.clientId] = end
			} else {
				return nil, nil, common.ErrMsgRegNotFound
			}
		}

		return nil, end, nil
	}).Resolve(s, endpoint)

	if _, err := p1.GetValue(); err == nil {
		return endpoint, nil
	} else {
		return nil, err
	}
}

func (s *ServerMgr) registAssist(conn net.Conn, msg *common.Msg) (*Assist, error) {
	defer msg.Free()

	regMsg := msg.GetReal().(*common.MsgAssistReg)
	ss := strings.Split(regMsg.Addr, ":")
	tcpIp := fmt.Sprintf("%v", conn.(*tls.Conn).RemoteAddr().(*net.TCPAddr).IP)
	regMsg.Addr = tcpIp + ":" + ss[len(ss)-1]
	ass := &Assist{tcpConn: conn, localAddr: regMsg.Addr, out: make(chan *common.Msg, common.MessageSeqSize), killed: make(chan struct{})}
	p1 := common.NewPromise(s).Then(func(pt common.PromiseTask, arg interface{}) (
		common.PromiseTask, interface{}, error) {
		s := pt.(*ServerMgr)
		ass := arg.(*Assist)
		if _, ok := s.assists[ass.localAddr]; !ok {
			s.assists[ass.localAddr] = ass
		}
		return nil, ass, nil
	}).Resolve(s, ass)

	if _, err := p1.GetValue(); err == nil {
		msg.Hdr.Type = common.MsgTypeAssistResp
		ass.out <- msg.Dup()
		return ass, nil
	} else {
		return nil, err
	}
}

func (s *ServerMgr) registTcpClient(conn net.Conn) (interface{}, error) {
	conn.SetReadDeadline(time.Now().Add(common.TcpTimeoutSec))
	if msg, merr := common.ReadTcpMsg(conn); merr == nil {
		defer msg.Free()
		if msg.GetReal() == nil {
			return nil, common.ErrMsgRealEmpty
		}
		if msg.Hdr.Type == common.MsgTypeReq {
			return s.registSpace(conn, msg.Dup())
		} else {
			return s.registAssist(conn, msg.Dup())
		}
	} else {
		return nil, merr
	}
}

func (s *ServerMgr) Start() {
	if err := s.listenerInit(); err != nil {
		log.Fatal(err)
	}

	if err := s.udpInit(); err != nil {
		log.Fatal(err)
	}

	defer s.tcpConn.Close()
	defer s.udpConn.Close()

	go s.handleTcpConn()
	go s.handleUdpConn()

	killable.Do(s, func() error {
		return s.Run()
	})
}

func (s *ServerMgr) runFor() error {
	ptask := s
	select {
	case <-s.Dying():
		return killable.ErrDying
	case msg := <-s.in:
		s.handleIncomeMsg(msg)
	case future, ok := <-ptask.Calls():
		if ok {
			future.Resp <- future.F(future, future.Arg)
		}
	case hook := <-s.HookAdd():
		if _, ok := s.hooks[hook.Seq]; ok {
			close(hook.Resp)
		} else {
			s.hooks[hook.Seq] = hook
		}
	case delSeq := <-s.HookDel():
		if hook, ok := s.hooks[delSeq]; ok {
			delete(s.hooks, delSeq)
			close(hook.Resp)
		} else {
			log.Println("error, hook cancel error, seq=", delSeq)
		}
	}

	return nil
}

func (s *ServerMgr) Run() error {
	for {
		if err := s.runFor(); err != nil {
			return err
		}
	}
}

func (s *ServerMgr) processUdpPing(msg *common.Msg) {
	defer msg.Free()

	/* if _, ok0 := msg.V.(*net.UDPAddr); !ok0 {
		log.Println("error for parse udp message")
		return
	} */

	msgPing := msg.GetReal().(*common.MsgUdpPong)
	if space, ok := s.spaces[msgPing.Space]; ok {
		if end, ok2 := space.endpoints[msgPing.ClientId]; ok2 {
			log.Println("msgPing.Type=", msgPing.Type, msg.V)
			switch msgPing.Type {
			case common.UdpPongTypeTwoServer, common.UdpPongTypeOneServer:
				//step0: redirect to assist
				msgResp := common.NewMsg(0)
				defer msgResp.Free()

				oldSeq := msg.Hdr.Seq
				newSeq := atomic.AddInt32(&s.seqMax, 1)

				msgResp.Hdr = common.MsgHdr{
					Seq:  uint16(newSeq),
					Type: common.MsgTypeP2pTest,
					Addr: uint8(msgPing.ClientId),
				}
				msgPing.Addr = fmt.Sprintf("%v", msg.V)

				//Always get the first
				var k string
				var v *Assist = nil
				for k, v = range s.assists {
					break
				}
				msgPing.Alter = k
				msgResp.Real = msgPing

				if msgResp.GetReal() != nil && v != nil && msgPing.Type == 0 {
					go func(msgR *common.Msg) {
						defer msgR.Free()

						hook := common.NewMsgHook(s, int(newSeq))
						select {
						case <-v.Dying():
						case v.out <- msgR.Dup():
						}

						if resp, err := hook.Wait(s, end.Dying(), 3*time.Second); err == nil {
							defer resp.Free()
							resp.Hdr.Seq = oldSeq

							log.Println("redirect ping ok", resp.Hdr.Type)
							select {
							case <-end.Dying():
							case end.out <- resp.Dup():
							}
						} else {
							if !killable.IsDying(s) && !killable.IsDying(end) {
								select {
								case <-end.Dying():
								case end.out <- msgR.Dup():
								}
								log.Println("redirec ping using old error", err)
							} else {
								log.Println("redirec ping error", err)
							}
						}

					}(msgResp.Dup())
				} else {
					log.Println("assist not found")
					msgResp.Real = msgPing
					msgResp.Hdr.Seq = oldSeq
					select {
					case <-end.Dying():
					case end.out <- msgResp.Dup():
					}
				}
			case common.UdpPongTypeFromAss:
				msg.Hdr.Type = common.MsgTypeUdpPong
				select {
				case <-end.Dying():
				case end.out <- msg.Dup():
				}
			default:
				log.Println("msgPing type error")
			}
		}
	}
}

func (s *ServerMgr) handleIncomeMsg(msg *common.Msg) {
	defer msg.Free()

	switch msg.Hdr.Type {
	//Handle ping message
	case common.MsgTypeUdpPing:
		s.processUdpPing(msg.Dup())

	/*case common.MsgTypeIPChange:
	end := msg.V.(*Endpoint)
	msg.V = nil
	if msg.Hdr.Addr == 0 {
		if space, ok := s.spaces[end.space]; ok {
			for _, p2pc := range space.endpoints {
				if p2pc.clientId != 0 {
					select {
					case <-p2pc.Dying():
					case p2pc.out <- msg.Dup():
					}
				}
			}
		}
	}*/
	case common.MsgTypeTcpPing:
		end := msg.V.(*Endpoint)
		msg.GetReal()
		select {
		case end.out <- msg.Dup():
		case <-end.Dying():
			return
		}
	case common.MsgTypeAssPing:
		ass := msg.V.(*Assist)
		msg.GetReal()
		//log.Println("got ass ping")
		select {
		case ass.out <- msg.Dup():
		case <-ass.Dying():
		}

	case common.MsgTypeAddrReq, common.MsgTypeAddrResp, common.MsgTypeDoShake:
		s.handleAddrMsg(msg.Dup())
	case common.MsgTypeP2pTest:
		seq := int(msg.Hdr.Seq)
		if hook, ok := s.hooks[seq]; ok {
			delete(s.hooks, seq)
			hook.Resp <- msg.Dup()
			close(hook.Resp)
			log.Println("process by hooks")
		}
	}
}

func (s *ServerMgr) handleAddrMsg(msg *common.Msg) {
	defer msg.Free()
	msgAddr := msg.GetReal().(*common.MsgReqAddr)

	from, to := 0, 0
	end := msg.V.(*Endpoint)
	log.Println("Endpoint got AddrMsg:", msg)

	if msg.Hdr.Type == common.MsgTypeAddrResp {
		end = msg.V.(*Endpoint)
		from = end.clientId
		to = msgAddr.From
	} else {
		from = end.clientId
		to = msgAddr.To
		msgAddr.From = from
	}

	msg.V = nil
	log.Println("from, to", from, to)

	if space, ok := s.spaces[end.space]; ok {
		if _, ok2 := space.endpoints[from]; ok2 {
			if toEnd, ok3 := space.endpoints[to]; ok3 {
				//redirect it to
				log.Println("redirect:", msg)
				select {
				case toEnd.out <- msg.Dup():
				case <-toEnd.Dying():
					return
				}
			}
		}
	}
}

func (s *ServerMgr) handleUdpConnFor(uBuf []byte) error {
	select {
	case <-s.Dying():
		return common.ErrMsgKilled
	default:
		n, addr, err := s.udpConn.ReadFromUDP(uBuf)
		if nil == err {
			msg := common.NewMsg(0)
			defer msg.Free()

			msg.V = addr
			if err2 := common.UnpackUdp(msg.Dup(), uBuf[:n]); err2 == nil {
				//log.Println("Got udp from", msg)
				/* switch msg.Hdr.Type {
				case common.MsgTypeUdpPing:
					o := msg.GetReal()
					if o != nil {
						msg.Hdr.Type = common.MsgTypeUdpPong
						ping := o.(*common.MsgUdpPong)
						ping.Addr = fmt.Sprintf("%v", addr)
						common.WriteUdpMsgServer(msg.Dup(), addr, s.udpConn)
					}
				default:
					log.Println("ignore udp message")
				} */
				if msg.GetReal() != nil {
					s.in <- msg.Dup()
				} else {
					log.Println("ignore empty message", msg.Hdr.Type)
				}

				//write back
				//uBuf[0] = common.MsgTypeUdpValid
				//s.udpConn.WriteToUDP(uBuf[:n], addr)

				return nil
			} else {
				return err2
			}
		}
	}
	return nil
}

func (s *ServerMgr) handleUdpConn() {
	uBuf := make([]byte, 1500)
	for {
		if err := s.handleUdpConnFor(uBuf); err != nil {
			log.Printf("handleUdpConn err:", err)
			return
		}
	}
}

func ServerMain() {
	serverMgr = serverInit()
	serverMgr.Start()
}
