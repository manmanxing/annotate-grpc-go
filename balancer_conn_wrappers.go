/*
 *
 * Copyright 2017 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package grpc

import (
	"fmt"
	"sync"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/internal/buffer"
	"google.golang.org/grpc/internal/channelz"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/resolver"
)

// scStateUpdate contains the subConn and the new state it changed to.
type scStateUpdate struct {
	sc    balancer.SubConn
	state connectivity.State
	err   error
}

// ccBalancerWrapper cc顶部的包装器，用于平衡器。它实现了 balancer.ClientConn 接口。
// balancer 对象是线程不安全的，然而日常的负载均衡信息同步是需要协调并发的，这部分并发协调的逻辑便位于 ccBalancerWrapper 对象之中
type ccBalancerWrapper struct {
	cc *ClientConn
	//balancerMu 在每次调用 balancer 时上锁保护
	balancerMu sync.Mutex
	balancer   balancer.Balancer
	//scBuffer 是接收 scStateUpdate 事件的入口，buffer.Unbounded 可以看做是一个不会阻塞的 chan，
	//scBuffer 的输入又是来自 ccBalancerWrapper 的回调函数 handleSubConnStateChange
	scBuffer *buffer.Unbounded
	//可以看做是关闭的标记位
	done *grpcsync.Event
	//mu 在每次访问 subConns 这个 ccBalancerWrapper 的内部状态时上锁保护
	mu       sync.Mutex
	subConns map[*acBalancerWrapper]struct{}
}

//在创建 ccBalancerWrapper 对象时，会跑一个新的 go ccb.watcher() 的 goroutine 用于实时监听 SubConnStateChange
func newCCBalancerWrapper(cc *ClientConn, b balancer.Builder, bopts balancer.BuildOptions) *ccBalancerWrapper {
	ccb := &ccBalancerWrapper{
		cc:       cc,
		scBuffer: buffer.NewUnbounded(),
		done:     grpcsync.NewEvent(),
		subConns: make(map[*acBalancerWrapper]struct{}),
	}
	go ccb.watcher()
	ccb.balancer = b.Build(ccb, bopts)
	return ccb
}


//每当 ClientConn 调用 handleSubConnStateChange 时，它将这个事件传递给 scBuffer，
//再在 go ccb.watcher() 里面调用 balancer 的 UpdateSubConnState 方法。
//为什么安排一个 scBuffer 而不是直接调用回调 UpdateSubConnState 呢？
//猜 handleSubConnStateChange 这个操作函数可能在个关键路径上，将操作异步化，不需要争用 balanceMu 这把锁。
func (ccb *ccBalancerWrapper) watcher() {
	for {
		select {
		case t := <-ccb.scBuffer.Get():
			ccb.scBuffer.Load()
			if ccb.done.HasFired() {
				break
			}
			ccb.balancerMu.Lock()
			su := t.(*scStateUpdate)
			ccb.balancer.UpdateSubConnState(su.sc, balancer.SubConnState{ConnectivityState: su.state, ConnectionError: su.err})
			ccb.balancerMu.Unlock()
		case <-ccb.done.Done():
		}

		if ccb.done.HasFired() {
			ccb.balancer.Close()
			ccb.mu.Lock()
			scs := ccb.subConns
			ccb.subConns = nil
			ccb.mu.Unlock()
			for acbw := range scs {
				ccb.cc.removeAddrConn(acbw.getAddrConn(), errConnDrain)
			}
			ccb.UpdateState(balancer.State{ConnectivityState: connectivity.Connecting, Picker: nil})
			return
		}
	}
}

func (ccb *ccBalancerWrapper) close() {
	ccb.done.Fire()
}

func (ccb *ccBalancerWrapper) handleSubConnStateChange(sc balancer.SubConn, s connectivity.State, err error) {
	// When updating addresses for a SubConn, if the address in use is not in
	// the new addresses, the old ac will be tearDown() and a new ac will be
	// created. tearDown() generates a state change with Shutdown state, we
	// don't want the balancer to receive this state change. So before
	// tearDown() on the old ac, ac.acbw (acWrapper) will be set to nil, and
	// this function will be called with (nil, Shutdown). We don't need to call
	// balancer method in this case.
	if sc == nil {
		return
	}
	ccb.scBuffer.Put(&scStateUpdate{
		sc:    sc,
		state: s,
		err:   err,
	})
}

func (ccb *ccBalancerWrapper) updateClientConnState(ccs *balancer.ClientConnState) error {
	ccb.balancerMu.Lock()
	defer ccb.balancerMu.Unlock()
	return ccb.balancer.UpdateClientConnState(*ccs)
}

func (ccb *ccBalancerWrapper) resolverError(err error) {
	ccb.balancerMu.Lock()
	ccb.balancer.ResolverError(err)
	ccb.balancerMu.Unlock()
}

func (ccb *ccBalancerWrapper) NewSubConn(addrs []resolver.Address, opts balancer.NewSubConnOptions) (balancer.SubConn, error) {
	if len(addrs) <= 0 {
		return nil, fmt.Errorf("grpc: cannot create SubConn with empty address list")
	}
	ccb.mu.Lock()
	defer ccb.mu.Unlock()
	if ccb.subConns == nil {
		return nil, fmt.Errorf("grpc: ClientConn balancer wrapper was closed")
	}
	ac, err := ccb.cc.newAddrConn(addrs, opts)
	if err != nil {
		return nil, err
	}
	acbw := &acBalancerWrapper{ac: ac}
	acbw.ac.mu.Lock()
	ac.acbw = acbw
	acbw.ac.mu.Unlock()
	ccb.subConns[acbw] = struct{}{}
	return acbw, nil
}

func (ccb *ccBalancerWrapper) RemoveSubConn(sc balancer.SubConn) {
	acbw, ok := sc.(*acBalancerWrapper)
	if !ok {
		return
	}
	ccb.mu.Lock()
	defer ccb.mu.Unlock()
	if ccb.subConns == nil {
		return
	}
	delete(ccb.subConns, acbw)
	ccb.cc.removeAddrConn(acbw.getAddrConn(), errConnDrain)
}

func (ccb *ccBalancerWrapper) UpdateAddresses(sc balancer.SubConn, addrs []resolver.Address) {
	acbw, ok := sc.(*acBalancerWrapper)
	if !ok {
		return
	}
	acbw.UpdateAddresses(addrs)
}

func (ccb *ccBalancerWrapper) UpdateState(s balancer.State) {
	ccb.mu.Lock()
	defer ccb.mu.Unlock()
	if ccb.subConns == nil {
		return
	}
	// Update picker before updating state.  Even though the ordering here does
	// not matter, it can lead to multiple calls of Pick in the common start-up
	// case where we wait for ready and then perform an RPC.  If the picker is
	// updated later, we could call the "connecting" picker when the state is
	// updated, and then call the "ready" picker after the picker gets updated.
	ccb.cc.blockingpicker.updatePicker(s.Picker)
	ccb.cc.csMgr.updateState(s.ConnectivityState)
}

func (ccb *ccBalancerWrapper) ResolveNow(o resolver.ResolveNowOptions) {
	ccb.cc.resolveNow(o)
}

func (ccb *ccBalancerWrapper) Target() string {
	return ccb.cc.target
}

// acBalancerWrapper is a wrapper on top of ac for balancers.
// It implements balancer.SubConn interface.
type acBalancerWrapper struct {
	mu sync.Mutex
	ac *addrConn
}

func (acbw *acBalancerWrapper) UpdateAddresses(addrs []resolver.Address) {
	acbw.mu.Lock()
	defer acbw.mu.Unlock()
	if len(addrs) <= 0 {
		acbw.ac.tearDown(errConnDrain)
		return
	}
	if !acbw.ac.tryUpdateAddrs(addrs) {
		cc := acbw.ac.cc
		opts := acbw.ac.scopts
		acbw.ac.mu.Lock()
		// Set old ac.acbw to nil so the Shutdown state update will be ignored
		// by balancer.
		//
		// TODO(bar) the state transition could be wrong when tearDown() old ac
		// and creating new ac, fix the transition.
		acbw.ac.acbw = nil
		acbw.ac.mu.Unlock()
		acState := acbw.ac.getState()
		acbw.ac.tearDown(errConnDrain)

		if acState == connectivity.Shutdown {
			return
		}

		ac, err := cc.newAddrConn(addrs, opts)
		if err != nil {
			channelz.Warningf(logger, acbw.ac.channelzID, "acBalancerWrapper: UpdateAddresses: failed to newAddrConn: %v", err)
			return
		}
		acbw.ac = ac
		ac.mu.Lock()
		ac.acbw = acbw
		ac.mu.Unlock()
		if acState != connectivity.Idle {
			ac.connect()
		}
	}
}

func (acbw *acBalancerWrapper) Connect() {
	acbw.mu.Lock()
	defer acbw.mu.Unlock()
	acbw.ac.connect()
}

func (acbw *acBalancerWrapper) getAddrConn() *addrConn {
	acbw.mu.Lock()
	defer acbw.mu.Unlock()
	return acbw.ac
}