package common

import (
	"errors"
	"fmt"

	"github.com/icholy/killable"
)

var (
	ErrFinished          = errors.New("killable: finished")
	ErrTargetDying       = errors.New("killable: targetDying")
	ErrPromiseEmpty      = errors.New("promise: emptyFuture")
	ErrPromiseNotResolve = errors.New("promise: notResolve")
	ErrPromiseResolving  = errors.New("promise: resolving")
	ErrPromisePDying     = errors.New("promise: parentDying")
)

type PromiseTask interface {
	killable.Killable
	Calls() chan *Future
	//Run() error
}

type Future struct {
	Resp chan *FutureResp
	Task PromiseTask
	Arg  interface{}
	F    FutureCallback
}

type FutureCallback func(in *Future, arg interface{}) *FutureResp
type FutureChainFunction func(pt PromiseTask, arg interface{}) (PromiseTask, interface{}, error)

type FutureResp struct {
	next PromiseTask
	v    interface{}
	err  error
}

//Not real promise but like promise
type Promise struct {
	killable.Killable
	parent   killable.Killable
	isPDying bool
	isDying  bool
	isSent   bool
	curr     int
	resp     chan *FutureResp
	v        interface{}
	err      error
	futures  []*Future
}

/* func (pt *PromiseTask) Run() error {
	for {
		select {
		case <-pt.Dying():
			return killable.ErrDying
		case future := <-pt.Calls:
			future.resp <- future.f(future, future.arg)
		}
	}

	return ErrFinished
} */

func NewPromise(parent killable.Killable) *Promise {
	isPDying := true
	if parent != nil {
		isPDying = killable.IsDying(parent)
	}
	return &Promise{
		parent:   parent,
		isPDying: isPDying,
		err:      ErrPromiseNotResolve,
		resp:     make(chan *FutureResp),
		futures:  make([]*Future, 0, 8),
		Killable: killable.New(),
	}
}

func (p *Promise) Then(f FutureChainFunction) *Promise {
	if p.isDying {
		return p
	}
	future := &Future{
		F: WrapFuture(f),
	}
	p.futures = append(p.futures, future)

	return p
}

func WrapFuture(f FutureChainFunction) FutureCallback {
	return func(in *Future, arg interface{}) *FutureResp {
		next, v, err := f(in.Task, in.Arg)
		return &FutureResp{
			next: next,
			v:    v,
			err:  err,
		}
	}
}

func (p *Promise) Resolve(pt PromiseTask, v interface{}) *Promise {
	if len(p.futures) == 0 || pt == nil || p.isDying || p.isPDying {
		return p
	}
	p.err = ErrPromiseResolving
	p.futures[0].Task = pt
	p.futures[0].Arg = v
	p.futures[0].Resp = p.resp
	killable.Go(p, func() error {
		return p.Run()
	})

	return p
}

func (p *Promise) GetValue() (interface{}, error) {
	if p.err == ErrPromiseNotResolve {
		return nil, p.err
	}
	<-p.Dead()

	return p.v, p.err
}

func (p *Promise) Run() error {
	var r *FutureResp
donep:
	for {
		future := p.futures[p.curr]
		select {
		case future.Task.Calls() <- future:
			p.isSent = true
		case <-p.Dying():
			p.isDying = true
			if p.isSent {
				//wait for resp or dying
				continue
			} else {
				p.err = killable.ErrDying
				break donep
			}
		case <-p.PDying():
			p.isPDying = true
			if p.isSent {
				continue
			} else {
				p.err = ErrPromisePDying
				break donep
			}
		case <-future.Task.Dying():
			p.err = ErrTargetDying
			break donep
		case r = <-future.Resp:
			p.isSent = false
		checkf:
			if p.isDying {
				//Killed myself, not kill it anymore
				p.err = killable.ErrDying
				break donep
			}
			if p.isPDying {
				//parent dying, killing myself
				p.err = ErrPromisePDying
				break donep
			}
			if r.err == nil && p.curr < (len(p.futures)-1) {
				p.curr++
				p.futures[p.curr].Resp = p.resp
				p.futures[p.curr].Arg = r.v
				p.futures[p.curr].Task = r.next
				if r.next != nil {
					continue
				}

				//r.next == nil, run future in this context
				future := p.futures[p.curr]
				r = future.F(future, future.Arg)
				for r.next == nil && r.err == nil && p.curr < (len(p.futures)-1) {
					p.curr++
					p.futures[p.curr].Resp = p.resp
					p.futures[p.curr].Arg = r.v
					p.futures[p.curr].Task = r.next

					future = p.futures[p.curr]
					r = future.F(future, future.Arg)
				}

				if r.next != nil && r.err == nil && p.curr < (len(p.futures)-1) {
					goto checkf
				}

				//err != nil || p.curr >= len(p.futures)
			}

			p.v = r.v
			if r.err == nil {
				p.err = nil
				return ErrFinished
			} else {
				p.err = r.err
			}
			break donep
		}
	}

	fmt.Printf("kill result=%v\n", p.err)
	return p.err
}

func (p *Promise) Dying() <-chan struct{} {
	if p.isDying {
		return nil
	}

	return p.Killable.Dying()
}

func (p *Promise) PDying() <-chan struct{} {
	if p.isPDying || p.parent == nil {
		return nil
	}

	return p.parent.Dying()
}
