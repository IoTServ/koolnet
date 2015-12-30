package client

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"net"

	"github.com/icholy/killable"
	//"encoding/json"
	//"strconv"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"strings"
	"sync/atomic"
	"time"

	common "../common"
)

const (
	P2pBegin      = 0
	P2pShaking    = 1
	P2pOk         = 2
	P2pConnAcking = 1
	P2pConnFin    = 2
	P2pConnNormal = 3
)

type ClientConfig struct {
	kind       string
	space      string
	password   string
	serverAddr string
	tunnels    []common.TunnelItem
	//pongAddr   string
	iv []byte
}

type Client struct {
	killable.Killable
	calls  chan *common.Future
	seqMax int32

	conf     *ClientConfig
	clientId int
	ip       string
	block    cipher.Block
	//Key: from clientId
	p2ps      map[int]*P2pClient
	serverUdp *net.UDPAddr
	//localAddrs []string
	//udpConn    *net.UDPConn
	tcpConn net.Conn
	hooks   map[int]*common.MsgHook
	in      chan *common.Msg
	out     chan *common.Msg
	hookAdd chan *common.MsgHook
	hookDel chan int
	newConn chan LocalConn
}

type LocalListen struct {
	s    net.Listener
	port int
	item common.TunnelItem
}

type LocalConn struct {
	l *LocalListen
	c net.Conn
}

func (c *Client) HookAdd() chan *common.MsgHook {
	return c.hookAdd
}

func (c *Client) HookDel() chan int {
	return c.hookDel
}

func configInit() *ClientConfig {
	var listen = flag.String("listen", "", "192.168.1.1:80;127.0.0.1:8800 192.168.1.1:22 0.0.0.0:8802")
	var saddr = flag.String("server", "ngrok.wang:18886", "server addr, like ngrok.wang:18886")
	var password = flag.String("password", "hello", "Your password")
	var name = flag.String("user", "user01", "Your username")
	var loglevel = flag.Int("level", 0, "Log level")
	flag.Usage = func() {
		fmt.Printf("Usage: p2ptunnel -listen=\"192.168.1.1:80;127.0.0.1:8800\" -server=ngrok.wang:18886 -password=hello\n\n")
		fmt.Printf("By Xiaobao, contact me at http://koolshare.cn\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	var iv []byte = nil
	kind := common.KindListen
	var items []common.TunnelItem = nil
	if *listen == "" {
		kind = "request"
	} else {
		items = make([]common.TunnelItem, 0, 8)
		for _, v1 := range strings.Split(*listen, " ") {
			v2 := strings.Split(v1, ";")
			if len(v2) == 2 {
				items = append(items, common.TunnelItem{ProxyAddr: v2[0], LocalAddr: v2[1]})
			} else {
				log.Fatal("listen string error")
			}
		}
		if len(items) == 0 {
			log.Fatal("listen string is not parsed")
		}
		var err error
		if iv, err = common.GenerateRandomBytes(aes.BlockSize); err != nil {
			log.Fatal("ganerate random bytes error", err)
		}
	}

	common.SetLevel(*loglevel)

	return &ClientConfig{
		kind:       kind,
		space:      *name,
		password:   *password,
		serverAddr: *saddr,
		tunnels:    items,
		//iv:         base64.StdEncoding.EncodeToString(iv),
		iv: iv,
	}
}

func newClient(conf *ClientConfig) *Client {
	return &Client{
		conf:     conf,
		Killable: killable.New(),
		calls:    make(chan *common.Future),

		p2ps: make(map[int]*P2pClient),
		//localAddrs: make([]string, 0, 8),
		hooks: make(map[int]*common.MsgHook),

		in:      make(chan *common.Msg, common.MessageSeqSize),
		out:     make(chan *common.Msg, common.MessageSeqSize),
		hookAdd: make(chan *common.MsgHook),
		hookDel: make(chan int),
		newConn: make(chan LocalConn),
	}
}

func (c *Client) Connect() error {
	var err error

	cert2_b, _ := Asset("client/cert2.pem")
	priv2_b, _ := Asset("client/cert2.key")
	priv2, _ := x509.ParsePKCS1PrivateKey(priv2_b)

	cert := tls.Certificate{
		Certificate: [][]byte{cert2_b},
		PrivateKey:  priv2,
	}

	config := tls.Config{Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true}
	c.tcpConn, err = tls.Dial("tcp", c.conf.serverAddr, &config)
	if err != nil {
		return err
	}
	return c.Regist()
}

func (c *Client) Regist() error {
	msg := common.NewMsg(0)
	defer msg.Free()
	//log.Println("regist, msgId", msg.Id)

	msg.Real = &common.MsgReg{
		Name:  c.conf.space,
		Pass:  c.conf.password,
		Kind:  c.conf.kind,
		Iv:    base64.StdEncoding.EncodeToString(c.conf.iv),
		Items: c.conf.tunnels}

	msg.Hdr = common.MsgHdr{
		Type: common.MsgTypeReq,
		Seq:  uint16(atomic.AddInt32(&c.seqMax, 1)),
	}

	c.tcpConn.SetWriteDeadline(time.Now().Add(common.TcpTimeoutSec))
	if err := common.WriteTcpMsg(c.tcpConn, msg.Dup()); err == nil {
		c.tcpConn.SetReadDeadline(time.Now().Add(common.TcpTimeoutSec))
		if rMsg, err2 := common.ReadTcpMsg(c.tcpConn); err2 == nil {
			defer rMsg.Free()
			resp := rMsg.GetReal().(*common.MsgRegResp)
			if resp != nil {
				c.clientId = resp.ClientId
				c.ip = resp.IP
				if c.clientId != 0 {
					c.conf.tunnels = resp.Items
					c.conf.iv, _ = base64.StdEncoding.DecodeString(resp.Iv)
				}
				common.Info("Got clientId:", c.clientId, "IP", c.ip)
				return nil
			} else {
				return common.ErrMsgSpaceExit
			}
		} else {
			return err2
		}
	} else {
		return err
	}
}

func (c *Client) initEncrypt() error {
	key := common.FixHash([]byte(c.conf.password))
	block, err := aes.NewCipher([]byte(key))
	c.block = block
	//if err == nil {
	//	c.encry = cipher.NewCFBEncrypter(block, c.conf.iv)
	//	c.decry = cipher.NewCFBDecrypter(block, c.conf.iv)
	//	return nil
	//} else {
	//	return err
	//}
	return err
}

func startClient(conf *ClientConfig) error {
	var err error

	c := newClient(conf)

	if err := c.Connect(); err != nil {
		return err
	}
	defer c.tcpConn.Close()

	if err = c.initEncrypt(); err != nil {
		return err
	}
	//log.Println("block:", c.block)
	//log.Println("iv:", c.conf.iv)

	if c.serverUdp, err = net.ResolveUDPAddr("udp", c.conf.serverAddr); err != nil {
		return err
	}

	//if c.udpConn, err = net.ListenUDP("udp", &net.UDPAddr{}); err != nil {
	//	return err
	//}
	//defer c.udpConn.Close()

	if c.clientId != 0 {
		c.listenLocal()
	}

	//c.resetLocalAddrs()
	//go c.udpReadLoop()
	go c.tcpReadLoop()
	go c.tcpWriteLoop()

	defer c.Close()
	common.Warn("Started")

	killable.Do(c, func() error {
		return c.Run()
	})

	return common.ErrMsgKilled
}

func (c *Client) Run() error {
	tick := time.NewTicker(common.TcpClientPingSec)
	defer tick.Stop()

	/* if c.clientId != 0 {
		c.requestP2p()
	} */

	//Gen a ping to server
	//c.udpPingServ()

	for {
		if err := c.RunFor(tick); err != nil {
			return err
		}
	}

	return common.ErrFinished
}

func (c *Client) Close() {
	//TODO kill all p2ps

	close(c.in)
	close(c.out)
	close(c.hookAdd)
	close(c.hookDel)

	for msg := range c.in {
		msg.Free()
	}
	for msg := range c.out {
		msg.Free()
	}
}

func (c *Client) Calls() chan *common.Future {
	return c.calls
}

func (c *Client) RunFor(tick *time.Ticker) error {
	select {
	case <-c.Dying():
		return killable.ErrDying
	case msg, ok := <-c.in:
		//Not msg.Dup() hear
		if ok {
			c.handleIncomeMsg(msg)
		} else {
			return common.ErrMsgKilled
		}
	case nc, ok := <-c.newConn:
		if ok {
			//Use fix timeout
			nc.c.SetReadDeadline(time.Now().Add(common.UdpP2pPingTimeout))
			if len(c.p2ps) == 0 {
				c.requestP2p()
			}
			p2pc, _ := c.p2ps[0]
			p2pc.NewConn(nc.c, nc.l.port, 0, false, c)
		}
	case future, ok := <-c.Calls():
		if ok {
			future.Resp <- future.F(future, future.Arg)
		} else {
			return common.ErrMsgKilled
		}
	case hook := <-c.HookAdd():
		if _, ok := c.hooks[hook.Seq]; ok {
			close(hook.Resp)
		} else {
			c.hooks[hook.Seq] = hook
		}
	case delSeq := <-c.HookDel():
		if hook, ok := c.hooks[delSeq]; ok {
			delete(c.hooks, delSeq)
			close(hook.Resp)
		} else {
			common.Warn("error, hook cancel error, seq=", delSeq)
		}
	case <-tick.C:
		//log.Println("udp ping")
		//c.udpPingServ()
		c.tcpPingServ()
	}

	return nil
}

func (c *Client) handleIncomeMsg(msg *common.Msg) {
	defer msg.Free()

	//log.Println("handleIncomeMsg", msg.Hdr.Type)
	switch msg.Hdr.Type {
	case common.MsgTypeIkcp:
		clientId := 0
		if c.clientId == 0 {
			clientId = int(msg.Hdr.Addr)
		} else {
			msg.Hdr.Addr = 0
		}
		if p2pc, ok := c.p2ps[clientId]; ok {
			p2pc.in <- msg.Dup()
		} else {
			common.Warn("ClientId not found", clientId, msg.Hdr.Type)
		}

	case common.MsgTypeHandshake, common.MsgTypeP2pPing,
		common.MsgTypeP2pPong, common.MsgTypeDoShake, common.MsgTypeUdpPing,
		common.MsgTypeUdpPong, common.MsgTypeP2pTest:
		//Process hook first
		seq := int(msg.Hdr.Seq)
		if hook, ok := c.hooks[seq]; ok {
			delete(c.hooks, seq)
			hook.Resp <- msg.Dup()
			close(hook.Resp)
			common.Info("process by hooks")
			return
		}

		clientId := int(msg.Hdr.Addr)
		if p2pc, ok := c.p2ps[clientId]; ok {
			p2pc.in <- msg.Dup()
		} else {
			common.Warn("ClientId not found", clientId, msg.Hdr.Type)
		}

	case common.MsgTypeTcpPing, common.MsgTypeTcpPong:
		//log.Println("tcp ping")

	case common.MsgTypeAddrReq, common.MsgTypeAddrResp:
		shake := msg.GetReal().(*common.MsgReqAddr)
		if shake.From == c.clientId {
			common.Info("Request from myself")
			if p2pc, ok := c.p2ps[0]; ok {
				p2pc.in <- msg.Dup()
			}

			return
		}

		if p2pc, ok := c.p2ps[shake.From]; ok {
			p2pc.in <- msg.Dup()
		} else {
			common.Info("create new p2pclient, from=", shake.From, "remoteaddrs", shake.FromAddrs)
			p2pc := &P2pClient{
				Killable: killable.New(),
				remoteId: shake.From,
				raddrs:   shake.FromAddrs,
				calls:    make(chan *common.Future),
				wMsg:     make(chan *common.MsgBuf, common.MessageSeqSize),
				in:       make(chan *common.Msg, common.MessageSeqSize),
			}
			c.p2ps[shake.From] = p2pc
			killable.Go(p2pc, func() error {
				return p2pc.RunC(c)
			})

			p2pc.in <- msg.Dup()
		}
	case common.MsgTypeClientQuit:
		clientId := msg.GetReal().(*common.MsgClientQuit).ClientId
		if p2pc, ok := c.p2ps[clientId]; ok {
			//Just kill it
			p2pc.Kill(common.ErrMsgSpaceExit)
		}

	default:
		common.Warn("pack not found", msg)
	}
}

func (c *Client) tcpReadLoopFor() error {
	conn := c.tcpConn
	conn.SetReadDeadline(time.Now().Add(common.TcpClientReadLoopSec))
	if msg, err := common.ReadTcpMsg(conn); err == nil {
		defer msg.Free()
		//log.Println("Client got msg:", msg.Id)

		select {
		case <-c.Dying():
			return common.ErrMsgKilled
		case c.in <- msg.Dup():
			return nil
		}
	} else {
		c.Kill(common.ErrMsgRead)
		return common.ErrMsgRead
	}
	return nil
}

func (c *Client) tcpReadLoop() {
	for {
		if err := c.tcpReadLoopFor(); err != nil {
			common.Warn("tcpReadLoop error: ", err)
			return
		}
	}
}

func (c *Client) tcpWriteLoopFor() error {
	conn := c.tcpConn
	select {
	case <-c.Dying():
		return common.ErrMsgKilled
	case msg, ok := <-c.out:
		if ok {
			defer msg.Free()
			conn.SetWriteDeadline(time.Now().Add(common.TcpClientWriteLoopSec))
			if err := common.WriteTcpMsg(conn, msg.Dup()); err == nil {
				return nil
			}
		}

		c.Kill(common.ErrMsgWrite)
		return common.ErrMsgWrite
	}

	return nil
}

func (c *Client) tcpWriteLoop() {
	for {
		if err := c.tcpWriteLoopFor(); err != nil {
			common.Warn("tcpWriteLoop error:", err)
			return
		}
	}
}

func (c *Client) requestP2p() {
	p2pc := &P2pClient{
		Killable: killable.New(),
		remoteId: 0,
		raddrs:   make([]string, 0, 8),
		cands:    make(map[string]string),
		calls:    make(chan *common.Future),
		in:       make(chan *common.Msg, common.MessageSeqSize),
		wMsg:     make(chan *common.MsgBuf, common.MessageSeqSize),
	}
	c.p2ps[0] = p2pc
	killable.Go(p2pc, func() error {
		return p2pc.RunC(c)
	})
}

func (c *Client) tcpPingServ() {
	msg := common.NewMsg(0)
	defer msg.Free()

	msg.Real = &common.MsgTcpPong{}
	msg.Hdr = common.MsgHdr{
		Type: common.MsgTypeTcpPing,
		Seq:  uint16(atomic.AddInt32(&c.seqMax, 1)),
	}
	c.out <- msg.Dup()
}

func (c *Client) RemoveClient(clientId int) {
	if p2pc, ok := c.p2ps[clientId]; ok {
		delete(c.p2ps, clientId)
		p2pc.Kill(common.ErrMsgKilled)
		<-p2pc.Dead()
		p2pc.Close()
	}
	common.Info("Client remove p2pclient", clientId)
}

func (c *Client) listenLocal() error {
	//log.Println("local listen:", c.conf.tunnels)
	for i, v := range c.conf.tunnels {
		if s, err := net.Listen("tcp", v.LocalAddr); err == nil {
			l := &LocalListen{
				s:    s,
				port: i,
				item: v,
			}

			go l.Run(c)
		} else {
			common.Warn("local listen error: ", err)
		}
	}

	return nil
}

func (l *LocalListen) Run(c *Client) error {
	for {
		select {
		case <-c.Dying():
			return common.ErrPromisePDying
		default:
			if conn, err := l.s.Accept(); err == nil {
				common.Info("got new local socks")
				l.newConn(conn, c)
			} else {
				common.Info("local accept error", err)
			}
		}
	}

	return common.ErrFinished
}

//TODO for client removed
func (l *LocalListen) newConn(conn net.Conn, c *Client) {
	/* conn.SetReadDeadline(time.Time{})
	p2pc, _ := c.p2ps[0]
	p2pc.NewConn(conn, l.port, 0, false, c) */
	select {
	case c.newConn <- LocalConn{l, conn}:
	case <-c.Dying():
	}
}

func ClientMain() {
	common.MsgPoolInit()
	conf := configInit()

	for {
		startClient(conf)

		if conf.kind == common.KindListen {
			common.Warn("waiting for 20s for reconnect")
			time.Sleep(20 * time.Second)
		} else {
			break
		}
	}
}
