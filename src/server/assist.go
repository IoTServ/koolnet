package server

import (
	"log"
	"net"
	"sync/atomic"
	"time"

	common "../common"
)

type Assist struct {
	tcpConn   net.Conn
	localAddr string
	out       chan *common.Msg
	killed    chan struct{}
	dying     int32
}

func (ass *Assist) Dying() <-chan struct{} {
	return ass.killed
}

//In server goroutine
func (ass *Assist) Close(s *ServerMgr) {
	ass.tcpConn.Close()
	if _, ok := s.assists[ass.localAddr]; ok {
		delete(s.assists, ass.localAddr)
		close(ass.out)
		log.Println("closed", ass.localAddr)
	}
}

func (ass *Assist) GoDying(s *ServerMgr) {
	if atomic.CompareAndSwapInt32(&ass.dying, 0, 1) {
		//Kill it first
		close(ass.killed)

		p1 := common.NewPromise(s).Then(func(pt common.PromiseTask, arg interface{}) (
			common.PromiseTask, interface{}, error) {
			s := pt.(*ServerMgr)
			ass := arg.(*Assist)

			ass.Close(s)

			return nil, ass, nil
		}).Resolve(s, ass)

		p1.GetValue()
	}
}

func (ass *Assist) writeLoopFor(s *ServerMgr) error {
	select {
	case <-s.Dying():
		return common.ErrMsgWrite
	case msg, ok := <-ass.out:
		if ok {
			//log.Println("writing")
			defer msg.Free()
			ass.tcpConn.SetWriteDeadline(time.Now().Add(common.TcpTimeoutSec))
			if err := common.WriteTcpMsg(ass.tcpConn, msg.Dup()); err == nil {
				return nil
			} else {
				log.Println("end writeloop error", err)
				ass.GoDying(s)
				return common.ErrMsgWrite
			}
		}
	}

	return nil
}

func (ass *Assist) readLoopFor(s *ServerMgr) error {
	conn := ass.tcpConn
	conn.SetReadDeadline(time.Now().Add(common.TcpTimeoutSec))
	if msg, err := common.ReadTcpMsg(conn); err == nil {
		defer msg.Free()

		msg.V = ass
		select {
		case <-s.Dying():
			return common.ErrPromisePDying
		case s.in <- msg.Dup():
		}
	} else {
		log.Println("readloop for error", err)
		ass.GoDying(s)
		return err
	}

	return nil
}

func (ass *Assist) WriteLoop(s *ServerMgr) error {
	for {
		if err := ass.writeLoopFor(s); err != nil {
			return err
		}
	}
}

func (ass *Assist) ReadLoop(s *ServerMgr) error {
	for {
		if err := ass.readLoopFor(s); err != nil {
			return err
		}
	}
}
