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

package base

import (
	"errors"
	"fmt"

	"google.golang.org/grpc/attributes"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/resolver"
)

var logger = grpclog.Component("balancer")

type baseBuilder struct {
	name          string
	pickerBuilder PickerBuilder
	config        Config
}

func (bb *baseBuilder) Build(cc balancer.ClientConn, opt balancer.BuildOptions) balancer.Balancer {
	bal := &baseBalancer{
		cc:            cc,
		pickerBuilder: bb.pickerBuilder,

		subConns: make(map[resolver.Address]subConnInfo),
		scStates: make(map[balancer.SubConn]connectivity.State),
		csEvltr:  &balancer.ConnectivityStateEvaluator{},
		config:   bb.config,
	}
	// Initialize picker to a picker that always returns
	// ErrNoSubConnAvailable, because when state of a SubConn changes, we
	// may call UpdateState with this picker.
	bal.picker = NewErrPicker(balancer.ErrNoSubConnAvailable)
	return bal
}

func (bb *baseBuilder) Name() string {
	return bb.name
}

type subConnInfo struct {
	subConn balancer.SubConn
	attrs   *attributes.Attributes
}

//在 balancer 创建之初的初始状态为 Idle，subConns 集合也为空。
//balancer 并不直接对连接做探活，连接的存活性等信息也来自于接口外部的 ClientConn 对象
type baseBalancer struct {
	cc            balancer.ClientConn
	pickerBuilder PickerBuilder

	csEvltr *balancer.ConnectivityStateEvaluator
	state   connectivity.State

	subConns map[resolver.Address]subConnInfo // `attributes` is stripped from the keys of this map (the addresses)
	scStates map[balancer.SubConn]connectivity.State
	picker   balancer.Picker
	config   Config

	resolverErr error // the last error reported by the resolver; cleared on successful resolution
	connErr     error // the last connection error; cleared upon leaving TransientFailure
}

func (b *baseBalancer) ResolverError(err error) {
	b.resolverErr = err
	if len(b.subConns) == 0 {
		b.state = connectivity.TransientFailure
	}

	if b.state != connectivity.TransientFailure {
		// The picker will not change since the balancer does not currently
		// report an error.
		return
	}
	b.regeneratePicker()
	b.cc.UpdateState(balancer.State{
		ConnectivityState: b.state,
		Picker:            b.picker,
	})
}

//更新客户端连接状态：处理名字解析得到的新地址、移除旧地址
func (b *baseBalancer) UpdateClientConnState(s balancer.ClientConnState) error {
	// TODO: handle s.ResolverState.ServiceConfig?
	if logger.V(2) {
		logger.Info("base.baseBalancer: got new ClientConn state: ", s)
	}
	// Successful resolution; clear resolver error and ensure we return nil.
	b.resolverErr = nil
	//用于快速查找 Addresses
	addrsSet := make(map[resolver.Address]struct{})
	//循环 target 最新一组已解析地址
	for _, a := range s.ResolverState.Addresses {
		aNoAttrs := a
		aNoAttrs.Attributes = nil//更新属性为 nil
		addrsSet[aNoAttrs] = struct{}{}
		if scInfo, ok := b.subConns[aNoAttrs]; !ok {
			// a 是一个新的地址，不存在于 b.subConns 中
			//为新发现的地址创建 SubConn，加入到 subConn 列表
			//标记 scState 状态为 Idle 并尝试建立连接，这里需要注意，Connect() 并非意味着连接的成功建立，而是一项异步操作。
			sc, err := b.cc.NewSubConn([]resolver.Address{a}, balancer.NewSubConnOptions{HealthCheckEnabled: b.config.HealthCheck})
			if err != nil {
				logger.Warningf("base.baseBalancer: failed to create new SubConn: %v", err)
				continue
			}
			b.subConns[aNoAttrs] = subConnInfo{subConn: sc, attrs: a.Attributes}
			b.scStates[sc] = connectivity.Idle
			sc.Connect()
		} else {
			// a 不是一个新地址，更新 subConn 的地址信息
			scInfo.attrs = a.Attributes
			b.subConns[aNoAttrs] = scInfo
			b.cc.UpdateAddresses(scInfo.subConn, []resolver.Address{a})
		}
	}
	//调用 ClientConn 的 RemoveSubConn，从自身的 subConns 集合中移除该 subConn，注意这里并没有移除 scState 的记录
	for a, scInfo := range b.subConns {
		// a was removed by resolver.
		if _, ok := addrsSet[a]; !ok {
			//RemoveSubConn 不意味着连接立即就关闭了
			//当后端实例被注销时，客户端可能仍有活跃请求存在，应当在活跃请求完成之后，再使连接进入 Shutdown 状态。
			b.cc.RemoveSubConn(scInfo.subConn)
			delete(b.subConns, a)
			// Keep the state of this sc in b.scStates until sc's state becomes Shutdown.
			// The entry will be deleted in UpdateSubConnState.
		}
	}
	// If resolver state contains no addresses, return an error so ClientConn
	// will trigger re-resolve. Also records this as an resolver error, so when
	// the overall state turns transient failure, the error message will have
	// the zero address information.
	if len(s.ResolverState.Addresses) == 0 {
		b.ResolverError(errors.New("produced zero addresses"))
		return balancer.ErrBadResolverState
	}
	return nil
}

// mergeErrors builds an error from the last connection error and the last
// resolver error.  Must only be called if b.state is TransientFailure.
func (b *baseBalancer) mergeErrors() error {
	// connErr must always be non-nil unless there are no SubConns, in which
	// case resolverErr must be non-nil.
	if b.connErr == nil {
		return fmt.Errorf("last resolver error: %v", b.resolverErr)
	}
	if b.resolverErr == nil {
		return fmt.Errorf("last connection error: %v", b.connErr)
	}
	return fmt.Errorf("last connection error: %v; last resolver error: %v", b.connErr, b.resolverErr)
}

// regeneratePicker takes a snapshot of the balancer, and generates a picker
// from it. The picker is
//  - errPicker if the balancer is in TransientFailure,
//  - built by the pickerBuilder with all READY SubConns otherwise.
func (b *baseBalancer) regeneratePicker() {
	if b.state == connectivity.TransientFailure {
		b.picker = NewErrPicker(b.mergeErrors())
		return
	}
	readySCs := make(map[balancer.SubConn]SubConnInfo)

	// Filter out all ready SCs from full subConn map.
	for addr, scInfo := range b.subConns {
		if st, ok := b.scStates[scInfo.subConn]; ok && st == connectivity.Ready {
			addr.Attributes = scInfo.attrs
			readySCs[scInfo.subConn] = SubConnInfo{Address: addr}
		}
	}
	b.picker = b.pickerBuilder.Build(PickerBuildInfo{ReadySCs: readySCs})
}

//侦听 SubConn 连接状态变化
//sc：子连接  state：当前子连接状态
func (b *baseBalancer) UpdateSubConnState(sc balancer.SubConn, state balancer.SubConnState) {
	s := state.ConnectivityState
	if logger.V(2) {
		logger.Infof("base.baseBalancer: handle SubConn state change: %p, %v", sc, s)
	}
	oldS, ok := b.scStates[sc]
	if !ok {
		if logger.V(2) {
			logger.Infof("base.baseBalancer: got state changes for an unknown SubConn: %p, %v", sc, s)
		}
		return
	}
	if oldS == connectivity.TransientFailure && s == connectivity.Connecting {
		// Once a subconn enters TRANSIENT_FAILURE, ignore subsequent
		// CONNECTING transitions to prevent the aggregated state from being
		// always CONNECTING when many backends exist but are all down.
		return
	}
	b.scStates[sc] = s
	switch s {
	case connectivity.Idle:
		sc.Connect()
	case connectivity.Shutdown:
		// When an address was removed by resolver, b called RemoveSubConn but
		// kept the sc's state in scStates. Remove state for this sc here.
		delete(b.scStates, sc)
	case connectivity.TransientFailure:
		// Save error to be reported via picker.
		b.connErr = state.ConnectionError
	}

	b.state = b.csEvltr.RecordTransition(oldS, s)

	//重新生成选择器判断逻辑：
	//-此sc已进入或准备就绪
	//-平衡器的聚合状态为TransientFailure（可能需要更新错误消息）
	if (s == connectivity.Ready) != (oldS == connectivity.Ready) ||
		b.state == connectivity.TransientFailure {
		b.regeneratePicker()
	}
    //最后通过 UpdateState 方法将 Picker 返回给 ClientConn 对象。
	b.cc.UpdateState(balancer.State{ConnectivityState: b.state, Picker: b.picker})
}

// Close is a nop because base balancer doesn't have internal state to clean up,
// and it doesn't need to call RemoveSubConn for the SubConns.
func (b *baseBalancer) Close() {
}

// NewErrPicker returns a Picker that always returns err on Pick().
func NewErrPicker(err error) balancer.Picker {
	return &errPicker{err: err}
}

// NewErrPickerV2 is temporarily defined for backward compatibility reasons.
//
// Deprecated: use NewErrPicker instead.
var NewErrPickerV2 = NewErrPicker

type errPicker struct {
	err error // Pick() always returns this err.
}

func (p *errPicker) Pick(info balancer.PickInfo) (balancer.PickResult, error) {
	return balancer.PickResult{}, p.err
}