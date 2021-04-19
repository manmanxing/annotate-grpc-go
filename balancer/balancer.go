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

//定义了用于gRPC中的负载平衡的API
package balancer

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"

	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/internal"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
)

var (
	//k:balancer的名称,v: 对应的balancer生成器
	m = make(map[string]Builder)
)

//如果构建器实现ConfigParser，则当解析程序接收到新的服务配置时，将调用ParseConfig，并且结果将在UpdateClientConnState中提供给Balancer。
//注意：此函数只能在初始化期间调用（即在init（）函数中），并且不是线程安全的。如果多个平衡器以相同的名称注册，则最后一个注册的平衡器将生效。
func Register(b Builder) {
	m[strings.ToLower(b.Name())] = b
}

//根据name删除对应的balancer生成器
//非线程安全的
func unregisterForTesting(name string) {
	delete(m, name)
}

func init() {
	//程序启动时，设置balancer的注销逻辑
	internal.BalancerUnregister = unregisterForTesting
}

//根据名称获取对应的生成器，注意是名称大小写不敏感
func Get(name string) Builder {
	if b, ok := m[strings.ToLower(name)]; ok {
		return b
	}
	return nil
}


//表示一个grpc子连接
//每个子连接包含了一系列的地址，grpc将会按顺序去连接他们，一旦一个连接成功，将停止尝试其余的连接。
//重新连接退避策略是应用于整个列表的地址，而不是单个。try_on_all_addresses -> backoff -> try_on_all_addresses.
//当子连接变为空闲时，除非 balancer 发起调用Connect，否则子连接将不会重新连接。
//当连接过程中发生错误，将会立即重试
type SubConn interface {
	//UpdateAddresses更新SubConn中使用的地址。
	//gRPC检查当前连接的地址是否仍在新列表中。如果在列表中，则将保持连接。
	//如果不在列表中，该连接将正常关闭，并创建一个新连接。
	UpdateAddresses([]resolver.Address)
	//尝试与subConn进行连接，注意：此连接为异步的
	Connect()
}

// NewSubConnOptions contains options to create new SubConn.
type NewSubConnOptions struct {
	// CredsBundle is the credentials bundle that will be used in the created
	// SubConn. If it's nil, the original creds from grpc DialOptions will be
	// used.
	//
	// Deprecated: Use the Attributes field in resolver.Address to pass
	// arbitrary data to the credential handshaker.
	CredsBundle credentials.Bundle
	// HealthCheckEnabled indicates whether health check service should be
	// enabled on this SubConn
	HealthCheckEnabled bool
}

// State 包含与gRPC ClientConn相关的balancer状态
type State struct {
	// State 包含balancer的连接状态，该状态用于确定ClientConn的状态。
	ConnectivityState connectivity.State
	// Picker 用于选择RPC的连接（SubConns）
	Picker Picker
}


//表示一个gRPC ClientConn
//这个 ClientConn 与 resolver 的 ClientConn 都是同一个对象（同一个指代的对象，有不同的接口定义，却还是同样的名字）
//负责创建、移除连接，也有权触发名字解析
type ClientConn interface {
	//由 balancer 调用，非阻塞的等待创建一个子连接
	NewSubConn([]resolver.Address, NewSubConnOptions) (SubConn, error)

	//从 ClientConn 移除 SubConn，该子连接将会被关闭
	RemoveSubConn(SubConn)

	//更新在SubConn中传递的地址。
	//gRPC检查当前连接的地址是否仍在新列表中。如果是这样，将保持连接。否则，该连接将被正常关闭，并且将创建一个新的连接。
	UpdateAddresses(SubConn, []resolver.Address)

	//通知gRPC，balancer的内部状态已更改
	//gRPC将更新ClientConn的连接状态，并将在新的Picker上调用Pick来选择新的SubConns。
	UpdateState(State)

	//由 balancer 调用，以通知gRPC进行名称解析。
	ResolveNow(resolver.ResolveNowOptions)

	//返回该 ClientConn 的 dial target
	Target() string
}

// BuildOptions contains additional information for Build.
type BuildOptions struct {
	// DialCreds is the transport credential the Balancer implementation can
	// use to dial to a remote load balancer server. The Balancer implementations
	// can ignore this if it does not need to talk to another party securely.
	DialCreds credentials.TransportCredentials
	// CredsBundle is the credentials bundle that the Balancer can use.
	CredsBundle credentials.Bundle
	// Dialer is the custom dialer the Balancer implementation can use to dial
	// to a remote load balancer server. The Balancer implementations
	// can ignore this if it doesn't need to talk to remote balancer.
	Dialer func(context.Context, string) (net.Conn, error)
	// ChannelzParentID is the entity parent's channelz unique identification number.
	ChannelzParentID int64
	// CustomUserAgent is the custom user agent set on the parent ClientConn.
	// The balancer should set the same custom user agent if it creates a
	// ClientConn.
	CustomUserAgent string
	// Target contains the parsed address info of the dial target. It is the same resolver.Target as
	// passed to the resolver.
	// See the documentation for the resolver.Target type for details about what it contains.
	Target resolver.Target
}

// Builder creates a balancer.
type Builder interface {
	// Build creates a new balancer with the ClientConn.
	Build(cc ClientConn, opts BuildOptions) Balancer
	// Name returns the name of balancers built by this builder.
	// It will be used to pick balancers (for example in service config).
	Name() string
}

// ConfigParser parses load balancer configs.
type ConfigParser interface {
	// ParseConfig parses the JSON load balancer config provided into an
	// internal form or returns an error if the config is invalid.  For future
	// compatibility reasons, unknown fields in the config should be ignored.
	ParseConfig(LoadBalancingConfigJSON json.RawMessage) (serviceconfig.LoadBalancingConfig, error)
}

// PickInfo contains additional information for the Pick operation.
type PickInfo struct {
	// FullMethodName is the method name that NewClientStream() is called
	// with. The canonical format is /service/Method.
	FullMethodName string
	// Ctx is the RPC's context, and may contain relevant RPC-level information
	// like the outgoing header metadata.
	Ctx context.Context
}

// DoneInfo contains additional information for done.
type DoneInfo struct {
	// Err is the rpc error the RPC finished with. It could be nil.
	Err error
	// Trailer contains the metadata from the RPC's trailer, if present.
	Trailer metadata.MD
	// BytesSent indicates if any bytes have been sent to the server.
	BytesSent bool
	// BytesReceived indicates if any byte has been received from the server.
	BytesReceived bool
	// ServerLoad is the load received from server. It's usually sent as part of
	// trailing metadata.
	//
	// The only supported type now is *orca_v1.LoadReport.
	ServerLoad interface{}
}

var (
	// ErrNoSubConnAvailable indicates no SubConn is available for pick().
	// gRPC will block the RPC until a new picker is available via UpdateState().
	ErrNoSubConnAvailable = errors.New("no SubConn is available")
	// ErrTransientFailure indicates all SubConns are in TransientFailure.
	// WaitForReady RPCs will block, non-WaitForReady RPCs will fail.
	//
	// Deprecated: return an appropriate error based on the last resolution or
	// connection attempt instead.  The behavior is the same for any non-gRPC
	// status error.
	ErrTransientFailure = errors.New("all SubConns are in TransientFailure")
)

// 包含与为RPC选择的连接有关的信息
type PickResult struct {
	// SubConn is the connection to use for this pick, if its state is Ready.
	// If the state is not Ready, gRPC will block the RPC until a new Picker is
	// provided by the balancer (using ClientConn.UpdateState).  The SubConn
	// must be one returned by ClientConn.NewSubConn.
	SubConn SubConn

	// Done is called when the RPC is completed.  If the SubConn is not ready,
	// this will be called with a nil parameter.  If the SubConn is not a valid
	// type, Done may not be called.  May be nil if the balancer does not wish
	// to be notified when the RPC completes.
	Done func(DoneInfo)
}

// TransientFailureError returns e.  It exists for backward compatibility and
// will be deleted soon.
//
// Deprecated: no longer necessary, picker errors are treated this way by
// default.
func TransientFailureError(e error) error { return e }

//gRPC使用Picker来选择SubConn以发送RPC。每当内部状态发生变化时，Balancer就会从其快照中生成一个新的选择器。
//gRPC使用的选择器可以通过ClientConn.UpdateState（）进行更新
type Picker interface {
	//选择不应该阻止。如果平衡器需要执行IO或任何阻塞或费时的工作来服务于此调用，则应返回ErrNoSubConnAvailable，
	//并且在更新Picker（使用ClientConn.UpdateState）时，gRPC将重复调用Pick。
	//有以下的错误：
	//如果错误是ErrNoSubConnAvailable，则gRPC将阻塞，直到平衡器提供新的选择器为止（使用ClientConn.UpdateState）。
	Pick(info PickInfo) (PickResult, error)
}

//该接口用于被 ClientConn 对象调用，从gRPC接收输入，管理SubConns，并收集和聚合连接状态。
//比如 resolver 解析得到的名字通知给 balancer
//它还会生成并更新 gRPC用来选择RPC的SubConns的 Picker
type Balancer interface {
	//当 ClientConn 的状态更改时，gRPC会调用它。
	//如果返回的错误是 ErrBadResolverState，则 ClientConn 将开始以指数回退的方式在活动名称解析器上调用 ResolveNow ，
	//直到对 UpdateClientConnState 的后续调用返回nil错误为止。当前将忽略任何其他错误。
	//参数 ClientConnState 便包含了名字解析结果的相关信息
	UpdateClientConnState(ClientConnState) error
	// ResolverError is called by gRPC when the name resolver reports an error.
	ResolverError(error)
	// UpdateSubConnState is called by gRPC when the state of a SubConn
	// changes.
	UpdateSubConnState(SubConn, SubConnState)
	// Close closes the balancer. The balancer is not required to call
	// ClientConn.RemoveSubConn for its existing SubConns.
	Close()
}

// SubConnState describes the state of a SubConn.
type SubConnState struct {
	//ClientConn 与 SubClientConn 都拥有一个 connectivity.State 类型的连接状态
	ConnectivityState connectivity.State
	// ConnectionError is set if the ConnectivityState is TransientFailure,
	// describing the reason the SubConn failed.  Otherwise, it is nil.
	ConnectionError error
}

// ClientConnState describes the state of a ClientConn relevant to the
// balancer.
type ClientConnState struct {
	ResolverState resolver.State
	// The parsed load balancing configuration returned by the builder's
	// ParseConfig method, if implemented.
	BalancerConfig serviceconfig.LoadBalancingConfig
}

// ErrBadResolverState may be returned by UpdateClientConnState to indicate a
// problem with the provided name resolver data.
var ErrBadResolverState = errors.New("bad resolver state")

// ConnectivityStateEvaluator takes the connectivity states of multiple SubConns
// and returns one aggregated connectivity state.
//
// It's not thread safe.
type ConnectivityStateEvaluator struct {
	numReady      uint64 // Number of addrConns in ready state.
	numConnecting uint64 // Number of addrConns in connecting state.
}

//记录subConn中发生的状态更改，并据此评估应该是什么聚合状态。
//
//balancer 的总体连接状态来自 RecordTransition 方法
//逻辑大约是只要有一个 Ready 连接，balancer 便属于 Ready 状态，然后只要有一个 Connecting 连接则 balancer 属于 Connecting 状态，其他则属于 TransientFailure 状态。
func (cse *ConnectivityStateEvaluator) RecordTransition(oldState, newState connectivity.State) connectivity.State {
	// Update counters.
	for idx, state := range []connectivity.State{oldState, newState} {
		updateVal := 2*uint64(idx) - 1 // -1 for oldState and +1 for new.
		switch state {
		case connectivity.Ready:
			cse.numReady += updateVal
		case connectivity.Connecting:
			cse.numConnecting += updateVal
		}
	}

	// Evaluate.
	if cse.numReady > 0 {
		return connectivity.Ready
	}
	if cse.numConnecting > 0 {
		return connectivity.Connecting
	}
	return connectivity.TransientFailure
}