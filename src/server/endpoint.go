package server

import (
	"fmt"
	"log"
	"net"
	"time"

	"github.com/icholy/killable"

	common "../common"
)

type Endpoint struct {
	killable.Killable
	calls    chan *common.Future
	space    string
	clientId int
	addrList []string
	lastPing time.Time

	//Can access in all goroutine
	spaceProp *SpaceProp

	out  chan *common.Msg
	conn net.Conn
}

func newEndpoint(regMsg *common.MsgReg, c net.Conn, s *ServerMgr) *Endpoint {
	clientId := -1
	var spaceProp *SpaceProp = nil
	if regMsg.Kind == common.KindListen {
		clientId = 0
		spaceProp = &SpaceProp{
			name:  regMsg.Name,
			pass:  regMsg.Pass,
			items: regMsg.Items,
			iv:    regMsg.Iv,
		}
	}

	log.Println("creating endpoint", clientId)
	return &Endpoint{
		Killable:  killable.New(),
		calls:     make(chan *common.Future),
		clientId:  clientId,
		space:     regMsg.Name,
		spaceProp: spaceProp,
		out:       make(chan *common.Msg),
		conn:      c,
	}
}

func (end *Endpoint) Calls() chan *common.Future {
	return end.calls
}

func (end *Endpoint) runSFor(s *ServerMgr) error {
	ptask := end
	select {
	case <-end.Dying():
		return killable.ErrDying
	case <-s.Dying():
		return common.ErrPromisePDying
	case future, ok := <-ptask.Calls():
		if ok {
			future.Resp <- future.F(future, future.Arg)
		}
	}
	return nil
}

func (end *Endpoint) RunS(s *ServerMgr) error {
	end.init(s)

	for {
		if err := end.runSFor(s); err != nil {
			return err
		}
	}
}

func (end *Endpoint) init(s *ServerMgr) {
	msg := common.NewMsg(0)
	defer msg.Free()

	msg.Hdr = common.MsgHdr{
		Type: common.MsgTypeRegResp,
	}
	msg.Real = &common.MsgRegResp{
		ClientId: end.clientId,
		Items:    end.spaceProp.items,
		Iv:       end.spaceProp.iv,
		IP:       fmt.Sprintf("%v", end.conn.RemoteAddr().(*net.TCPAddr).IP),
		Status:   "ok"}
	end.conn.SetReadDeadline(time.Time{})
	end.out <- msg.Dup()
}

func (end *Endpoint) writeLoopFor(s *ServerMgr) error {
	select {
	case <-end.Dying():
		return common.ErrMsgWrite
	case m, ok := <-end.out:
		//m is dup when send, free it hear
		if ok {
			defer m.Free()
			//log.Println("should free")
			end.conn.SetWriteDeadline(time.Now().Add(common.TcpTimeoutSec))
			if err := common.WriteTcpMsg(end.conn, m.Dup()); err == nil {
				return nil
			} else {
				log.Println("end writeloop error", err)
				end.Kill(common.ErrMsgWrite)
				return common.ErrMsgWrite
			}
		}
	}
	return nil
}

func (end *Endpoint) WriteLoop(s *ServerMgr) {
	for {
		if err := end.writeLoopFor(s); err == nil {
			continue
		} else {
			return
		}
	}
}

func (end *Endpoint) readLoopFor(s *ServerMgr) error {
	conn := end.conn
	conn.SetReadDeadline(time.Now().Add(common.TcpTimeoutSec))
	if msg, err := common.ReadTcpMsg(conn); err == nil {
		defer msg.Free()
		msg.V = end
		//log.Println("Endpoint got msg:", msg)
		select {
		//ServerMgr always quit after end, it should never close in before end
		case s.in <- msg.Dup():
		case <-end.Dying():
			return common.ErrPromisePDying
		}
	} else {
		log.Println("endpoint readloop error", err)
		end.Kill(common.ErrMsgRead)
		return err
	}
	return nil
}

func (end *Endpoint) ReadLoop(s *ServerMgr) error {
	for {
		if err := end.readLoopFor(s); err != nil {
			return err
		}
	}
}

//Close after dead in server.go
func (end *Endpoint) Close(s *ServerMgr) {
	//remote space and close all the other client from space
	common.NewPromise(s).Then(func(pt common.PromiseTask, arg interface{}) (common.PromiseTask, interface{}, error) {
		end := arg.(*Endpoint)
		mgr := pt.(*ServerMgr)
		//Better for GC
		end.spaceProp = nil

		log.Println(fmt.Sprintf("delete space=%s clientId=%d from server", end.space, end.clientId))
		if end.clientId == 0 {
			if space, ok := mgr.spaces[end.space]; ok {
				for k, end2 := range space.endpoints {
					end2.Kill(common.ErrMsgSpaceExit)
					delete(space.endpoints, k)
				}
				delete(mgr.spaces, end.space)
			}
		} else {
			//Notify the listener that requestor quit
			if space, ok := mgr.spaces[end.space]; ok {
				delete(space.endpoints, end.clientId)
				if listen_end, ok2 := space.endpoints[0]; ok2 {
					rMsg := common.NewMsg(0)
					defer rMsg.Free()

					rMsg.Hdr = common.MsgHdr{
						Type: common.MsgTypeClientQuit,
					}
					rMsg.Real = &common.MsgClientQuit{ClientId: end.clientId}
					listen_end.out <- rMsg.Dup()
				}
			}
		}

		close(end.calls)
		close(end.out)
		for msg := range end.out {
			//Free the buffer message
			log.Println("free all hear")
			msg.Free()
		}

		return nil, nil, nil
	}).Resolve(s, end)
}
