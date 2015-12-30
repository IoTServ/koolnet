package client

import (
	"bytes"
	"encoding/binary"
	"net"
	"sync/atomic"
	"time"

	"github.com/icholy/killable"

	common "../common"
)

type P2pConn struct {
	killable.Killable
	tcpConn   net.Conn
	innerPort int
	tcpPort   int
	isProxy   bool
	rLen      int32
	wLen      int32
	hdr       common.MsgHdr
	in        chan *common.MsgBuf
	wMsg      chan *common.MsgBuf
}

func (p *P2pConn) RunFor(p2pc *P2pClient, c *Client) error {
	select {
	case <-p2pc.Dying():
		return common.ErrPromisePDying
	case <-p.Dying():
		//Maybe not run, if other place return error
		p.GoDying(p2pc, c)
		return killable.ErrDying
	case mb, ok := <-p.in:
		if ok {
			defer mb.Free()

			switch mb.Type {
			case common.MsgTypeData:
				//log.Println("data, mbSize", mb.Size)
				size := mb.Size
				wsize := 0
			SEND_LOOP:
				for {
					if n, err := p.tcpConn.Write(mb.GetReal()[wsize:]); err == nil && n == (size-wsize) {
						break SEND_LOOP
					} else if err == nil {
						wsize += n
					} else {
						//Response fin message
						p.hdr.Type = common.MsgTypeFin
						rmb := common.NewMsgBuf()
						defer rmb.Free()
						//log.Println("send_loop", rmb.Id)

						bf := bytes.NewBuffer(make([]byte, 0, common.MsgHdrSize))
						binary.Write(bf, binary.BigEndian, p.hdr)
						copy(rmb.GetBuf(), bf.Bytes())
						rmb.Size = common.MsgHdrSize
						select {
						case <-p2pc.Dying():
						case p2pc.wMsg <- rmb.Dup():
						}

						p.GoDying(p2pc, c)
						return common.ErrMsgWrite
					}
				}
				atomic.AddInt32(&p.wLen, int32(wsize))

			case common.MsgTypeSynOk:
				//Always requestor
				common.Info("go tcpReadLoop")
				go p.tcpReadLoop(p2pc, c)
			case common.MsgTypeFin:
				p.GoDying(p2pc, c)
				return common.ErrMsgWrite
			}
		}
	}

	return nil
}

func (p *P2pConn) Init(p2pc *P2pClient, c *Client) error {
	p.hdr = common.MsgHdr{
		Type: common.MsgTypeData,
		Addr: uint8(c.clientId),
		Port: uint16(p.tcpPort),
		Seq:  uint16(p.innerPort),
	}

	mb := common.NewMsgBuf()
	defer mb.Free()
	//log.Println("Init p2pconn", mb.Id)

	var synHdr common.MsgHdr
	bf := bytes.NewBuffer(make([]byte, 0, common.MsgHdrSize))
	if p.isProxy {
		go p.tcpReadLoop(p2pc, c)

		synHdr = common.MsgHdr{
			Type: common.MsgTypeSynOk,
			Port: uint16(p.tcpPort),
			Seq:  uint16(p.innerPort),
		}
	} else {
		//Send MsgTypeSync
		synHdr = common.MsgHdr{
			Type: common.MsgTypeSyn,
			Port: uint16(p.tcpPort),
			Seq:  uint16(p.innerPort),
		}
	}

	binary.Write(bf, binary.BigEndian, synHdr)
	copy(mb.GetBuf(), bf.Bytes())
	mb.Size = common.MsgHdrSize
	select {
	case <-p2pc.Dying():
		return common.ErrMsgKilled
	case p2pc.wMsg <- mb.Dup():
	}
	return nil
}

func (p *P2pConn) Run(p2pc *P2pClient, c *Client) error {
	if err := p.Init(p2pc, c); err != nil {
		return err
	}

	for {
		if err := p.RunFor(p2pc, c); err != nil {
			common.Info("P2pConn error", err)
			return err
		}
	}

	return nil
}

func (p *P2pConn) GoDying(p2pc *P2pClient, c *Client) {
	common.NewPromise(p2pc).Then(func(pt common.PromiseTask, arg interface{}) (common.PromiseTask, interface{}, error) {
		p2pc.RemoveConn(p)
		return nil, nil, nil
	}).Resolve(p2pc, p)
}

func (p *P2pConn) Close() {
	p.tcpConn.Close()
	close(p.in)
	close(p.wMsg)
	for msg := range p.in {
		msg.Free()
	}
	for msg := range p.wMsg {
		msg.Free()
	}
	common.Info("p2pconn closed recv", p.rLen, p.wLen)
}

func (p *P2pConn) tcpReadLoopFor(p2pc *P2pClient, c *Client) error {
	waitCount := 0
	for {
		waitsnd := atomic.LoadInt32(&p2pc.waitSend)
		if waitsnd > 1024 && waitCount <= 3 {
			common.Warn("slow down reading circle", p2pc.waitSend)
			select {
			case <-p.Dying():
				break
			case <-time.After(800 * time.Millisecond):
			}
			waitCount++
		} else {
			break
		}
	}

	mb := common.NewMsgBuf()
	defer mb.Free()
	//log.Println("tcpReadLoopFor", mb.Id)

	//Setup hdr
	bf := bytes.NewBuffer(make([]byte, 0, common.MsgHdrSize))
	binary.Write(bf, binary.BigEndian, p.hdr)
	copy(mb.GetBuf(), bf.Bytes())

	n, err := p.tcpConn.Read(mb.GetBuf()[common.MsgHdrSize:])
	mb.Size = n + common.MsgHdrSize

	atomic.AddInt32(&p.rLen, int32(n))

	if nil == err {
		select {
		case <-p2pc.Dying():
			return common.ErrMsgKilled
		case p2pc.wMsg <- mb.Dup():
		}
	} else {
		bf.Reset()
		p.hdr.Type = common.MsgTypeFin
		binary.Write(bf, binary.BigEndian, p.hdr)
		copy(mb.GetBuf(), bf.Bytes())
		mb.Size = common.MsgHdrSize
		common.Info("tcpRead error", mb.GetReal())

		select {
		//Already dying, ignore Fin message
		case <-p.Dying():
		case <-p2pc.Dying():
		case p2pc.wMsg <- mb.Dup():
		}
		p.Kill(common.ErrMsgRead)
		return common.ErrMsgRead
	}
	return nil
}

func (p *P2pConn) tcpReadLoop(p2pc *P2pClient, c *Client) {
	p.tcpConn.SetReadDeadline(time.Now().Add(common.UdpP2pPingTimeout))

	for {
		if err := p.tcpReadLoopFor(p2pc, c); err != nil {
			return
		}
	}
}
