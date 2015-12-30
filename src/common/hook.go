package common

import (
	"time"
)

type MsgHook struct {
	Seq  int
	Resp chan *Msg
}

type MsgHooker interface {
	HookAdd() chan *MsgHook
	HookDel() chan int
	Dying() <-chan struct{}
}

func NewMsgHook(c MsgHooker, seq int) *MsgHook {
	mh := &MsgHook{Seq: seq, Resp: make(chan *Msg, 1)}
	select {
	case c.HookAdd() <- mh:
		return mh
	case <-c.Dying():
		return nil
	}
}

//Have to free the msg
func (mhook *MsgHook) Wait(c MsgHooker, killed <-chan struct{}, timesec time.Duration) (*Msg, error) {
	select {
	case <-killed:
	case <-c.Dying():
	case <-time.After(timesec):
		select {
		case <-c.Dying():
		case c.HookDel() <- mhook.Seq:
		}
	case resp, ok := <-mhook.Resp:
		if ok {
			//Have to check if resp is error.
			return resp, nil
		}
	}
	for m := range mhook.Resp {
		defer m.Free()
	}
	return nil, ErrPromisePDying
}
