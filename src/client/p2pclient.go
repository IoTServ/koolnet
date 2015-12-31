package client

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"net"
	"time"

	"github.com/icholy/killable"

	common "../common"
	ikcp "../ikcp"
)

const (
	UPADTE_TICK = 20 * time.Millisecond
	MASTER      = 0
	SLAVE       = 1
)

var (
	ErrMsgContinue = errors.New("message: continue")
)

type StartInfo struct {
	pong1   string
	pong2   string
	udpConn *net.UDPConn
}

/* process: A as Request, B as Proxy
new conn from Request
A init;
A startReq; send common.MsgTypeAddrReq to server; server redirect to Proxy B
B create p2pclient; init; send common.MsgTypeAddrResp to server; server redirect to A; startReq
A Got MsgTypeAddrResp; choose role, role is SLAVE shakeAll to B; send common.MsgTypeDoShake to B
B Got MsgTypeDoShake; choose role, role is MASTER shakeAll to A; if SLAVE send common.MsgTypeDoShake to A
A <--> B
*/

type P2pClient struct {
	killable.Killable
	calls chan *common.Future
	state int
	//other side of clientId
	remoteId int
	client   *Client
	laddrs   []string
	raddrs   []string
	in       chan *common.Msg
	wMsg     chan *common.MsgBuf

	udpConn    *net.UDPConn
	pongAddr   string
	pongAddr2  string
	pubAddr    string
	pubAddr2   string
	local      *net.UDPAddr
	remote     *net.UDPAddr
	cands      map[string]string
	choose     string
	shakeCount int
	startReq   chan *StartInfo
	role       int

	//ikcp
	waitSend int32
	kcp      *ikcp.Ikcpcb
	busy     int32
	lastPing time.Time
	lastSeq  int

	//P2pConn
	portMap map[int]*P2pConn
}

func (p2pc *P2pClient) Calls() chan *common.Future {
	return p2pc.calls
}

func (p2pc *P2pClient) Close() {
	for _, p := range p2pc.portMap {
		//The p is already killed hear
		delete(p2pc.portMap, p.tcpPort)
		<-p.Dead()
		p.Close()
		common.Info("Client close, RemoveConn ", p.tcpPort)
	}

	p2pc.client = nil

	close(p2pc.startReq)
	close(p2pc.calls)
	close(p2pc.in)
	close(p2pc.wMsg)

	for msg := range p2pc.in {
		msg.Free()
	}

	for msg := range p2pc.wMsg {
		msg.Free()
	}
}

//ms
func iclock() int32 {
	return int32((time.Now().UnixNano() / 1000000) & 0xffffffff)
}

func udp_output(buf []byte, _len int32, kcp *ikcp.Ikcpcb, user interface{}) int32 {
	p := user.(*P2pClient)
	//common.Info("udp_output", buf[:_len])
	common.WriteUdpConnOnly(p.client.block, p.client.conf.iv, buf[:_len], p.remote, p.udpConn)
	return 0
}

func (p2pc *P2pClient) NewConn(conn net.Conn, port int, tPort int, isProxy bool, c *Client) {
	tcpPort := tPort
	if 0 == tcpPort {
		tcpPort = conn.RemoteAddr().(*net.TCPAddr).Port
	}

	p2pConn := &P2pConn{
		Killable:  killable.New(),
		isProxy:   isProxy,
		innerPort: port,
		tcpPort:   tcpPort,
		tcpConn:   conn,
		rlist:     make([]*common.MsgBuf, 0, 16),
		in:        make(chan *common.MsgBuf, 16),
		wMsg:      make(chan *common.MsgBuf, 16),
	}

	//Use promise to run in p2pclient thread context, than modify the portMap only in p2pclient context
	common.NewPromise(c).Then(func(pt common.PromiseTask, arg interface{}) (common.PromiseTask, interface{}, error) {
		p2pc := pt.(*P2pClient)
		p2pConn := arg.(*P2pConn)
		p2pc.portMap[p2pConn.tcpPort] = p2pConn
		common.Info("NewConn len", len(p2pc.portMap))
		killable.Go(p2pConn, func() error {
			return p2pConn.Run(p2pc, c)
		})
		return nil, nil, nil
	}).Resolve(p2pc, p2pConn)

	common.Info("tcpPort=", p2pConn.tcpPort, "innertPort=", port)
}

func (p2pc *P2pClient) Init(c *Client) error {
	p2pc.client = c
	p2pc.pubAddr = ""

	p2pc.cands = make(map[string]string)
	p2pc.choose = ""
	var conv uint32
	if c.clientId == 0 {
		//proxy
		conv = common.MsgTypeIkcp | (uint32(p2pc.remoteId) << 16)
	} else {
		//requestor
		conv = common.MsgTypeIkcp | (uint32(c.clientId) << 16)
	}
	p2pc.kcp = ikcp.Ikcp_create(conv, p2pc)
	p2pc.kcp.Output = udp_output
	ikcp.Ikcp_wndsize(p2pc.kcp, 128, 128)
	ikcp.Ikcp_nodelay(p2pc.kcp, 1, 20, 2, 1)
	ikcp.Ikcp_setmtu(p2pc.kcp, 1460)
	p2pc.lastPing = time.Time{}

	p2pc.portMap = make(map[int]*P2pConn)

	if udpConn, err := net.ListenUDP("udp", &net.UDPAddr{}); err != nil {
		common.Warn("create udpsock error")
		return err
	} else {
		p2pc.udpConn = udpConn
	}

	p2pc.startReq = make(chan *StartInfo)

	go p2pc.udpReadLoop(c)

	return nil
}

func (p2pc *P2pClient) p2pDiscovery(udpConn *net.UDPConn, c *Client) error {
	sinfo := &StartInfo{}

	if udpConn == nil {
		udpConn = p2pc.udpConn
	}

	seq := atomic.AddInt32(&c.seqMax, 1)
	hook := common.NewMsgHook(c, int(seq))
	udpPingServ(udpConn, c.serverUdp, common.UdpPongTypeTwoServer, seq, c)
	pong, alter := p2pc.tcpPongRecv(hook, c)
	common.Info("alter=", alter)

	pong2 := ""
	if alter != "" {
		alterUdp, _ := net.ResolveUDPAddr("udp", alter)
		seq = atomic.AddInt32(&c.seqMax, 1)
		hook = common.NewMsgHook(c, int(seq))
		udpPingServ(udpConn, alterUdp, common.UdpPongTypeFromAss, seq, c)
		pong2, _ = p2pc.tcpPongRecv(hook, c)
	}

	udpConn.SetReadDeadline(time.Time{})
	sinfo.pong1 = pong
	sinfo.pong2 = pong2
	sinfo.udpConn = udpConn

	/* if sinfo.pong1 != sinfo.pong2 {
		if udpConn, err := net.ListenUDP("udp", &net.UDPAddr{}); err != nil {
			common.Info("create udpsock error")
			return err
		} else {
			udpConn.SetReadDeadline(time.Time{})
			sinfo.udpConn = udpConn
			laddrs := p2pc.udpAddrs(udpConn)
			ss1 := strings.Split(pong, ":")
			ss2 := strings.Split(laddrs[0], ":")
			sinfo.pong1 = pong[:len(pong)-len(ss1[len(ss1)-1])] + ss2[len(ss2)-1]
		}
	} */

	common.Info("init pong", pong, pong2)

	p2pc.startReq <- sinfo
	return nil
}

func getIPPort(s string) (ip string, port int) {
	var err error
	ss := strings.Split(s, ":")
	ip = ""
	port = 0
	if port, err = strconv.Atoi(ss[len(ss)-1]); err != nil {
		return
	}
	ip = strings.Join(ss[:len(ss)-1], ":")
	return
}

func (p2pc *P2pClient) chooseRole(c *Client) {
	pubIP1, pubPort1 := getIPPort(p2pc.pubAddr)
	pubIP2, pubPort2 := getIPPort(p2pc.pubAddr2)
	locIP1, locPort1 := getIPPort(p2pc.pongAddr)
	locIP2, locPort2 := getIPPort(p2pc.pongAddr2)

	locNAT2, remoteNAT2 := false, false
	if (pubPort1 != 0 && pubPort2 != 0) && (p2pc.pubAddr == p2pc.pubAddr2 || pubIP1 != pubIP2) {
		remoteNAT2 = true
	}
	if (locPort1 != 0 && locPort2 != 0) && (p2pc.pongAddr == p2pc.pongAddr2 || locIP1 != locIP2) {
		locNAT2 = true
	}

	defer func() {
		common.Warn("Choose role as:", p2pc.pongAddr, p2pc.pongAddr2, p2pc.pubAddr, p2pc.pubAddr2, p2pc.role, locNAT2, remoteNAT2)
	}()

	if c.clientId == 0 && locNAT2 {
		//Local_Proxy_node + Local_NAT2 = master
		p2pc.role = MASTER
		return
	}

	if c.clientId != 0 && remoteNAT2 {
		//Local_Request_node + Remote_NAT2 = SLAVE
		p2pc.role = SLAVE
		return
	}

	if c.clientId == 0 && remoteNAT2 {
		//Local_Proxy_node + Remote_NAT2 + Local_NAT3 = SLAVE
		p2pc.role = SLAVE
		return
	}

	if c.clientId != 0 && locNAT2 {
		//Local_Request_node + Remote_nat3 + Local_nat2 = MASTER
		p2pc.role = MASTER
		return
	}

	if c.clientId == 0 {
		p2pc.role = MASTER
	} else {
		p2pc.role = SLAVE
	}
}

func (p2pc *P2pClient) RunC(c *Client) error {
	updateChan := time.NewTicker(UPADTE_TICK)
	pingTick := time.NewTicker(common.UdpP2pPingSec)
	defer updateChan.Stop()
	defer pingTick.Stop()

	p2pc.Init(c)

	go p2pc.p2pDiscovery(nil, c)

	for {
		if err := p2pc.RunCFor(updateChan, pingTick); err != nil {
			return err
		}
	}
}

func (p2pc *P2pClient) RunCFor(updateChan *time.Ticker, pingTick *time.Ticker) error {
	c := p2pc.client

	select {
	case <-c.Dying():
		return common.ErrPromisePDying
	case <-p2pc.Dying():
		p2pc.DoDying(c)
		return killable.ErrDying
	case future := <-p2pc.Calls():
		future.Resp <- future.F(future, future.Arg)
	case <-updateChan.C:
		ikcp.Ikcp_update(p2pc.kcp, uint32(iclock()))
	case msg, ok := <-p2pc.in:
		if ok {
			//Message income to p2pc, Not message free hear
			p2pc.handleMsgIn(msg, c)
		}
	case mb, ok := <-p2pc.wMsg:
		if ok {
			//Write msg buffer
			defer mb.Free()
			//common.Info("GetBuf", mb.GetReal())
			ikcp.Ikcp_send(p2pc.kcp, mb.GetReal(), mb.Size)
			waitsnd := ikcp.Ikcp_waitsnd(p2pc.kcp)
			atomic.StoreInt32(&p2pc.waitSend, waitsnd)
			//common.Info("waitsnd", waitsnd)
		}

	case <-pingTick.C:
		if p2pc.state == P2pShaking {
			p2pc.shakeAll(c)
			if p2pc.shakeCount > 5 {
				p2pc.DoDying(c)
				return common.ErrMsgShakeTimeout
			}
		} else if p2pc.state == P2pOk {
			now := time.Now()
			t1 := p2pc.lastPing.Add(common.UdpP2pPingTimeout)
			t2 := p2pc.lastPing.Add(common.UdpP2pPingClose)

			//now > t1 && now < t2
			if now.After(t1) && now.Before(t2) {
				//Ping timeout but pong not timeout
				common.Info("p2p ping")
				p2pc.p2pPing(c)
			} else if now.After(t2) { //now > t2
				common.Info("pong timeout, restart shaking")
				//Restart for shaking, set to begin first
				p2pc.state = P2pBegin
				if udpConn, err := net.ListenUDP("udp", &net.UDPAddr{}); err != nil {
					common.Warn("create udpsock error")
					p2pc.DoDying(c)
					return err
				} else {
					go p2pc.p2pDiscovery(udpConn, c)
				}
			}
		}
	case sinfo := <-p2pc.startReq:
		//Set proxier for ready recv message
		p2pc.state = P2pShaking

		p2pc.pongAddr = sinfo.pong1
		p2pc.pongAddr2 = sinfo.pong2
		if sinfo.udpConn != nil && sinfo.udpConn != p2pc.udpConn {
			//change udpConn, restart udpReadLoop
			p2pc.udpConn.Close()
			p2pc.udpConn = sinfo.udpConn
			sinfo.udpConn = nil
			go p2pc.udpReadLoop(c)
		}

		p2pc.resetLocalAddrs(c)
		if c.clientId == 0 {
			p2pc.genAddrResp(c)

			p2pc.chooseRole(c)
			if SLAVE == p2pc.role {
				//ShakeAll right now
				p2pc.state = P2pShaking
				p2pc.shakeAll(c)
			}
			common.Info("got from themself", p2pc.laddrs, p2pc.raddrs, p2pc.pubAddr, p2pc.pubAddr2)
		} else {
			p2pc.requestAddr(c)
		}
	}

	return nil
}

func (p2pc *P2pClient) handleMsgIn(msg *common.Msg, c *Client) {
	defer msg.Free()

	//common.Info("msgIn", msg.Hdr.Type)
	switch msg.Hdr.Type {
	case common.MsgTypeIkcp:
		//Ikcp data, update lastPing first
		p2pc.lastPing = time.Now()
		p2pc.handleIkcpData(msg.Dup(), c)

	case common.MsgTypeAddrReq:
		if p2pc.state != P2pOk || (p2pc.state == P2pOk && time.Since(p2pc.lastPing) >= common.UdpP2pPingClose) {
			shake := msg.GetReal().(*common.MsgReqAddr)
			p2pc.raddrs = shake.FromAddrs
			p2pc.pubAddr = shake.FromPub
			p2pc.pubAddr2 = shake.FromPub2
			common.Info("got pub", p2pc.pubAddr, p2pc.pubAddr2)

			//pongAddr not ok hear
			if p2pc.state == P2pShaking {
				p2pc.genAddrResp(c)
			}
		}

	case common.MsgTypeAddrResp:
		addrResp := msg.GetReal().(*common.MsgReqAddr)
		if p2pc.state != P2pOk || (p2pc.state == P2pOk && time.Since(p2pc.lastPing) > common.UdpP2pPingTimeout) {
			p2pc.choose = ""
			p2pc.raddrs = addrResp.Addrs
			p2pc.pubAddr = addrResp.ToPub
			p2pc.pubAddr2 = addrResp.ToPub2
			p2pc.resetLocalAddrs(c)
			common.Info("got from myself", p2pc.laddrs, p2pc.raddrs, p2pc.pubAddr, p2pc.pubAddr2)

			p2pc.chooseRole(c)
			if SLAVE == p2pc.role {
				//ShakeAll right now
				p2pc.state = P2pShaking
				p2pc.shakeAll(c)

				//Additions for nats
				p2pc.updateAddrs()
			}

			//Notify remote for shaking, clientId != 0
			common.Info("Notify remote for shaking", c.clientId, p2pc.laddrs)
			sMsg := common.NewMsg(0)
			defer sMsg.Free()
			sMsg.Real = &common.MsgReqAddr{
				From:      c.clientId,
				To:        0,
				FromAddrs: p2pc.laddrs,
			}
			sMsg.Hdr = common.MsgHdr{
				Addr: uint8(c.clientId),
				Seq:  uint16(atomic.AddInt32(&c.seqMax, 1)),
				Type: common.MsgTypeDoShake,
			}
			c.out <- sMsg.Dup()
		}

	case common.MsgTypeDoShake:
		//TODO is this ok?
		if p2pc.state == P2pOk {
			common.Info("current is p2pok, ignore")
			return
		}

		if p2pc.role == MASTER {
			//Proxy and Master
			p2pc.state = P2pShaking
			p2pc.shakeAll(c)

			//Additions for some nats
			p2pc.updateAddrs()
		}

		common.Info("Response notify remote for shake", c.clientId)
		if c.clientId == 0 {
			msg.GetReal()
			msg.Real = &common.MsgReqAddr{
				From:      0,
				To:        int(msg.Hdr.Addr),
				FromAddrs: p2pc.laddrs,
			}
			//Set currect addr
			msg.Hdr.Addr = uint8(c.clientId)
			c.out <- msg.Dup()
		}

	case common.MsgTypeHandshake:
		p2pc.handleShakeingMsg(msg.Dup(), c)

	case common.MsgTypeP2pPing:
		p2pc.lastPing = time.Now()
		if p2pc.remote != nil {
			rMsg := common.NewMsg(0)
			defer rMsg.Free()

			rMsg.Real = &common.MsgP2pPong{From: c.clientId}
			rMsg.Hdr = common.MsgHdr{
				Seq:  uint16(atomic.AddInt32(&c.seqMax, 1)),
				Type: common.MsgTypeP2pPong,
				Addr: uint8(c.clientId),
			}
			common.WriteUdpMsg(c.block, c.conf.iv, rMsg.Dup(), p2pc.remote, p2pc.udpConn)
		}
	case common.MsgTypeP2pPong:
		p2pc.lastPing = time.Now()
	default:
		common.Warn("msg type not found")
	}
}

func (p2pc *P2pClient) genAddrResp(c *Client) {
	msg := common.NewMsg(0)
	defer msg.Free()

	msg.Hdr = common.MsgHdr{
		Type: common.MsgTypeAddrResp,
		Addr: uint8(c.clientId),
	}

	msg.Real = &common.MsgReqAddr{
		From:      p2pc.remoteId,
		To:        c.clientId,
		Addrs:     p2pc.laddrs,
		ToPub:     p2pc.pongAddr,
		ToPub2:    p2pc.pongAddr2,
		FromAddrs: p2pc.raddrs,
		FromPub:   p2pc.pubAddr,
		FromPub2:  p2pc.pubAddr2,
	}
	c.out <- msg.Dup()
}

func (p2pc *P2pClient) handleIkcpData(msg *common.Msg, c *Client) {
	defer msg.Free()

	//log.Println("ikcp data", msg.GetOrigin().Len(), msg.GetOrigin().Bytes())
	ikcp.Ikcp_input(p2pc.kcp, msg.GetOrigin().Bytes(), msg.GetOrigin().Len())
	for {
		if err := p2pc.tryToRecvFor(c); err != ErrMsgContinue {
			return
		}
	}
}

func (p2pc *P2pClient) tryToRecvFor(c *Client) error {
	mb := common.NewMsgBuf()
	defer mb.Free()
	//log.Println("tryToRecvFor", mb.Id)

	hr := ikcp.Ikcp_recv(p2pc.kcp, mb.GetBuf(), common.MsgSizeBig)
	if hr > 0 {
		var hdr common.MsgHdr
		bf := bytes.NewBuffer(mb.GetBuf()[:common.MsgHdrSize])
		binary.Read(bf, binary.BigEndian, &hdr)
		mb.Start = common.MsgHdrSize
		mb.Size = (int(hr) - common.MsgHdrSize)

		mb.Type = int(hdr.Type)
		fromTcpPort := int(hdr.Port)
		toInnerPort := int(hdr.Seq)
		//log.Println("kcp recv", fromTcpPort, toInnerPort, mb.Type)

		if p, ok := p2pc.portMap[fromTcpPort]; ok {
			p.in <- mb.Dup()
		} else {
			switch hdr.Type {
			case common.MsgTypeSyn:
				proxyAddrConf := c.conf.tunnels[toInnerPort].ProxyAddr
				//Use goroutine to dial tcp
				go func(proxyAddr string) {
					tcpConn, err := net.DialTimeout("tcp", proxyAddr, common.TcpTimeoutSec)
					if nil == err {
						tcpConn.SetReadDeadline(time.Time{})
						//After initial complete, tell remote we are ready
						p2pc.NewConn(tcpConn, toInnerPort, fromTcpPort, true, c)
					} else {
						common.Warn("p2pclient dial error", fromTcpPort, err)

						//Response error
						mb := common.NewMsgBuf()
						defer mb.Free()
						hdr = common.MsgHdr{
							Type: common.MsgTypeSynErr,
							Port: uint16(toInnerPort),
							Seq:  uint16(fromTcpPort),
						}
						bf := bytes.NewBuffer(make([]byte, 0, common.MsgHdrSize))
						binary.Write(bf, binary.BigEndian, hdr)
						copy(mb.GetBuf(), bf.Bytes())
						mb.Size = common.MsgHdrSize
						select {
						case <-p2pc.Dying():
						case p2pc.wMsg <- mb.Dup():
						}
					}
				}(proxyAddrConf)
			case common.MsgTypeFin:
			default:
				common.Warn("message ignore", hdr.Type, fromTcpPort)
			}
		}

		return ErrMsgContinue
	} else {
		return nil
	}
}

func (p2pc *P2pClient) handleShakeingMsg(msg *common.Msg, c *Client) {
	defer msg.Free()

	p2pc.lastSeq = int(msg.Hdr.Seq)
	if p2pc.state == P2pShaking {
		shake := msg.GetReal().(*common.MsgHandshake)
		fromClient := int(msg.Hdr.Addr)
		if fromClient == c.clientId {
			common.Info("Ignore the message from myself")
			return
		}

		fromAddr := msg.V.(*net.UDPAddr)
		var sid string
		if c.clientId < p2pc.remoteId {
			sid = fmt.Sprintf("%s;%v", shake.RAddr, fromAddr)
		} else {
			sid = fmt.Sprintf("%v;%s", fromAddr, shake.RAddr)
		}
		p2pc.cands[shake.RAddr] = sid
		common.Info("handleShakeingMsg sid", sid)
		if shake.Choose != "" && p2pc.choose == shake.Choose {
			//handshake ok
			p2pc.state = P2pOk
			p2pc.onChooseOk(c)
			p2pc.lastPing = time.Now()
			common.Info("choose ok", p2pc.choose, p2pc.local, p2pc.remote)

			if c.clientId < p2pc.remoteId {
				// send resp to requester
				cands := make([]string, 0, 8)
				for _, v := range p2pc.cands {
					cands = append(cands, v)
				}
				rShake := &common.MsgHandshake{Cands: cands}
				p2pc.shakeOne(fromAddr, shake.Choose, rShake, c)
			} else {
				//send ping right now
				p2pc.p2pPing(c)
			}
		} else {
			//Not ok, so continue
			if c.clientId > p2pc.remoteId {
				choose := ""
				cands := make([]string, 0, 8)
				for _, v := range shake.Cands {
					vv := strings.Split(v, ";")
					if _, ok := p2pc.cands[vv[1]]; ok {
						cands = append(cands, v)
						if choose == "" || choose > v {
							//choose the little one
							choose = v
						}
					}
				}

				rShake := &common.MsgHandshake{Cands: cands}
				p2pc.choose = shake.Choose
				p2pc.shakeOne(fromAddr, choose, rShake, c)
			} else {
				//write only cands
				if shake.Choose != "" {
					p2pc.choose = shake.Choose
				}
				cands := make([]string, 0, 8)
				for _, v := range p2pc.cands {
					cands = append(cands, v)
				}

				rShake := &common.MsgHandshake{Cands: cands}
				p2pc.shakeOne(fromAddr, p2pc.choose, rShake, c)
			}
		}
	}
}

func (p2pc *P2pClient) p2pPing(c *Client) {
	//TODO send message in p2pc channel

	msg := common.NewMsg(0)
	defer msg.Free()

	msg.Real = &common.MsgP2pPing{From: c.clientId}
	msg.Hdr = common.MsgHdr{
		Seq:  uint16(atomic.AddInt32(&c.seqMax, 1)),
		Type: common.MsgTypeP2pPing,
		Addr: uint8(c.clientId),
	}
	common.WriteUdpMsg(c.block, c.conf.iv, msg.Dup(), p2pc.remote, p2pc.udpConn)
}

func (p2pc *P2pClient) shakeAll(c *Client) {
	p2pc.shakeCount++

	shake := &common.MsgHandshake{}
	for i := 0; i < 3; i++ {
		for _, v := range p2pc.raddrs {
			addr, _ := net.ResolveUDPAddr("udp", v)
			p2pc.shakeOne(addr, "", shake, c)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p2pc *P2pClient) shakeOne(addr *net.UDPAddr, choose string, shake *common.MsgHandshake, c *Client) {
	shake.RAddr = fmt.Sprintf("%v", addr)
	shake.Choose = choose
	clientId := c.clientId

	msg := common.NewMsg(0)
	defer msg.Free()

	msg.Real = shake
	msg.Hdr = common.MsgHdr{
		Seq:  uint16(atomic.AddInt32(&c.seqMax, 1)),
		Type: common.MsgTypeHandshake,
		Addr: uint8(clientId),
	}

	common.Info("shakeOne To", addr)
	common.WriteUdpMsg(c.block, c.conf.iv, msg.Dup(), addr, p2pc.udpConn)
}

func (p2pc *P2pClient) onChooseOk(c *Client) {
	p2pc.shakeCount = 0

	// Choose ok now
	s := strings.Split(p2pc.choose, ";")
	if c.clientId == 0 {
		p2pc.local, _ = net.ResolveUDPAddr("udp", s[0])
		p2pc.remote, _ = net.ResolveUDPAddr("udp", s[1])
	} else {
		p2pc.local, _ = net.ResolveUDPAddr("udp", s[1])
		p2pc.remote, _ = net.ResolveUDPAddr("udp", s[0])
	}
}

func (p2pc *P2pClient) DoDying(c *Client) {
	for _, v := range p2pc.portMap {
		common.Info("kill connection", v.tcpPort)
		v.Kill(common.ErrMsgKilled)
	}

	common.NewPromise(c).Then(func(pt common.PromiseTask, arg interface{}) (common.PromiseTask, interface{}, error) {
		c := pt.(*Client)
		remoteId := arg.(int)
		c.RemoveClient(remoteId)
		return nil, nil, nil
	}).Resolve(c, p2pc.remoteId)
}

func (p2pc *P2pClient) RemoveConn(p *P2pConn) {
	p.Kill(common.ErrMsgKilled)
	if _, ok := p2pc.portMap[p.tcpPort]; ok {
		//Delete from map than close it
		delete(p2pc.portMap, p.tcpPort)
		//Wait for dead
		<-p.Dead()
		//Close all channel
		p.Close()
	}
	common.Info("RemoveConn ", p.tcpPort, "RemoteConn len", len(p2pc.portMap))
}

//About udp
func (p2pc *P2pClient) udpReadLoopFor(udpConn *net.UDPConn, c *Client, rBuf []byte) error {
	msg := common.NewMsg(0)
	defer msg.Free()
	//log.Println("udpReadLoop", msg.Id)

	if n, addr, err := udpConn.ReadFromUDP(rBuf); err == nil {
		common.Info("Got n=", n, "from", addr)
		if err2 := common.UnpackUdpMsg(c.block, c.conf.iv, msg.Dup(), rBuf[:n]); err2 != nil {
			//Ignore this msg
			//log.Println("unpack udp message error", err2)
			return nil
		}
		if msg.Hdr.Type == common.MsgTypeIkcp || msg.GetReal() != nil {
			msg.V = addr

			select {
			case <-c.Dying():
				return common.ErrMsgKilled
			case <-p2pc.Dying():
				return common.ErrMsgKilled
			case c.in <- msg.Dup():
				return nil
			}
		} else {
			common.Warn("ignore empty p2p message", msg.Hdr.Type)
		}
	} else {
		common.Warn("udpreadloop error", err)
		return common.ErrMsgRead
	}

	return nil
}

func (p2pc *P2pClient) udpReadLoop(c *Client) {
	rBuf := make([]byte, 1500)
	for {
		if err := p2pc.udpReadLoopFor(p2pc.udpConn, c, rBuf); err != nil {
			return
		}
	}
}

func (p2pc *P2pClient) requestAddr(c *Client) {
	sMsg := common.NewMsg(0)
	defer sMsg.Free()

	//log.Println("Pong recv, msgId", sMsg.Id)

	sMsg.Real = &common.MsgReqAddr{
		From:      c.clientId,
		To:        0,
		FromPub:   p2pc.pongAddr,
		FromPub2:  p2pc.pongAddr2,
		FromAddrs: p2pc.laddrs,
	}
	//log.Println("requestAddr", sMsg.Real)

	if c.clientId != 0 {
		sMsg.Hdr = common.MsgHdr{
			Addr: uint8(c.clientId),
			Seq:  uint16(atomic.AddInt32(&c.seqMax, 1)),
			Type: common.MsgTypeAddrReq,
		}
	}

	//Send message
	c.out <- sMsg.Dup()
}

func (p2pc *P2pClient) udpAddrs(udpConn *net.UDPConn) []string {
	laddr := udpConn.LocalAddr().(*net.UDPAddr)
	laddrs := make([]string, 0, 10)
	switch {
	case laddr.IP.IsLoopback():
		common.Warn("Connecting over loopback not supported")
	case laddr.IP.IsUnspecified():
		addrs, err := net.InterfaceAddrs()
		if err != nil {
			break
		}

		for _, addr := range addrs {
			ip, ok := addr.(*net.IPNet)
			if ok && ip.IP.IsGlobalUnicast() && ip.IP.To4() != nil {
				laddrs = append(laddrs, fmt.Sprintf("%v", &net.UDPAddr{IP: ip.IP, Port: laddr.Port}))
			}
		}

	default:
		laddrs = append(laddrs, fmt.Sprintf("%v", laddr))
	}
	return laddrs
}

func (p2pc *P2pClient) resetLocalAddrs(c *Client) error {
	laddrs := p2pc.udpAddrs(p2pc.udpConn)
	set := make(map[string]int)
	for _, v := range laddrs {
		set[v] = 1
	}

	if len(laddrs) > 0 {
		ss := strings.Split(laddrs[0], ":")
		ip := c.ip + ":" + ss[len(ss)-1]
		if _, ok := set[ip]; !ok {
			laddrs = append(laddrs, ip)
			set[ip] = 1
		}
	}

	if p2pc.pongAddr != "" {
		if _, ok := set[p2pc.pongAddr]; !ok {
			laddrs = append(laddrs, p2pc.pongAddr)
			set[p2pc.pongAddr] = 1
		}
	}

	p2pc.laddrs = laddrs
	common.Warn("resetLocalAddrs:", laddrs)
	return nil
}

func udpPingServ(udpConn *net.UDPConn, addr *net.UDPAddr, t int, seq int32, c *Client) {
	msg := common.NewMsg(0)
	defer msg.Free()

	//log.Println("udpPing msgId", msg.Id)

	msg.Real = &common.MsgUdpPong{
		Type:     t,
		Space:    c.conf.space,
		ClientId: c.clientId,
	}
	msg.Hdr = common.MsgHdr{
		Seq:  uint16(seq),
		Type: common.MsgTypeUdpPing,
		Addr: uint8(c.clientId),
	}
	common.WriteUdpMsgServer(msg.Dup(), addr, udpConn)
}

func (p2pc *P2pClient) tcpPongRecv(hook *common.MsgHook, c *Client) (string, string) {
	if resp, err := hook.Wait(c, p2pc.Dying(), 5*time.Second); err == nil {
		defer resp.Free()
		pong, ok := resp.GetReal().(*common.MsgUdpPong)
		if ok {
			return pong.Addr, pong.Alter
		}
	}
	return "", ""
}

func Abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func (p2pc *P2pClient) updateAddrs() {
	if p2pc.pubAddr == "" && len(p2pc.raddrs) != 0 {
		p2pc.pubAddr = p2pc.raddrs[len(p2pc.raddrs)-1]
	}

	raddrs := make(map[string]int)
	for _, s := range p2pc.raddrs {
		raddrs[s] = 1
	}

	pubIP1, pubPort1 := getIPPort(p2pc.pubAddr)
	pubIP2, pubPort2 := getIPPort(p2pc.pubAddr2)
	diff := Abs(pubPort2 - pubPort1)
	if pubIP1 == pubIP2 && pubPort2 != pubPort1 && pubPort1 > 8 && diff < 128 {
		if diff < 8 {
			if pubPort2 > pubPort1 {
				for i := pubPort2 + 1; i <= pubPort2+2*diff; i++ {
					raddrs[pubIP1+":"+strconv.Itoa(i)] = 1
				}
			} else {
				for i := pubPort1 - 2*diff; i < pubPort1; i++ {
					raddrs[pubIP1+":"+strconv.Itoa(i)] = 1
				}
			}
		} else {
			if pubPort2 > pubPort1 {
				raddrs[pubIP1+":"+strconv.Itoa(pubPort2+diff)] = 1
				raddrs[pubIP1+":"+strconv.Itoa(pubPort2+diff*2)] = 1
			} else {
				raddrs[pubIP1+":"+strconv.Itoa(pubPort1-diff)] = 1
				raddrs[pubIP1+":"+strconv.Itoa(pubPort1-diff*2)] = 1
			}
		}
	}

	if pubIP1 != pubIP2 || p2pc.role == MASTER {
		raddrs[p2pc.pubAddr2] = 1
	}

	p2pc.raddrs = p2pc.raddrs[len(p2pc.raddrs):]
	for k, _ := range raddrs {
		p2pc.raddrs = append(p2pc.raddrs, k)
	}

	common.Warn("update raddrs", p2pc.raddrs)
}
