/*
 *
 * Copyright 2014 gRPC authors.
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
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/base"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/internal/backoff"
	"google.golang.org/grpc/internal/channelz"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/internal/grpcutil"
	iresolver "google.golang.org/grpc/internal/resolver"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
	"google.golang.org/grpc/status"

	_ "google.golang.org/grpc/balancer/roundrobin"           // To register roundrobin.
	_ "google.golang.org/grpc/internal/resolver/dns"         // To register dns resolver.
	_ "google.golang.org/grpc/internal/resolver/passthrough" // To register passthrough resolver.
	_ "google.golang.org/grpc/internal/resolver/unix"        // To register unix resolver.
)

const (
	// 一个连接完成的最短时间
	minConnectTimeout = 20 * time.Second
	// must match grpclbName in grpclb/grpclb.go
	//负载均衡名称
	grpclbName = "grpclb"
)

var (
	// ErrClientConnClosing indicates that the operation is illegal because
	// the ClientConn is closing.
	//
	// Deprecated: this error should not be relied upon by users; use the status
	// code of Canceled instead.
	//被弃用，用code.Canceled代替
	//客户端正在关闭错误
	ErrClientConnClosing = status.Error(codes.Canceled, "grpc: the client connection is closing")
	// errConnDrain indicates that the connection starts to be drained and does not accept any new RPCs.
	//表示连接开始被耗尽并且不接受任何新的RPC
	errConnDrain = errors.New("grpc: the connection is drained")
	// errConnClosing indicates that the connection is closing.
	//表示连接正在关闭。
	errConnClosing = errors.New("grpc: the connection is closing")
	// invalidDefaultServiceConfigErrPrefix is used to prefix the json parsing error for the default
	// service config.
	//表示提供的默认服务配置非法
	invalidDefaultServiceConfigErrPrefix = "grpc: the provided default service config is invalid"
)

//下面的错误来自Dial和DialContext
var (
	//表示没有为ClientConn设置任何传输安全性。用户应设置一个或显式调用WithInsecure DialOption以禁用安全性。
	errNoTransportSecurity = errors.New("grpc: no transport security set (use grpc.WithInsecure() explicitly or set credentials)")
	//凭证不得与单独的TransportCredentials一起使用
	errTransportCredsAndBundle = errors.New("grpc: credentials.Bundle may not be used with individual TransportCredentials")
	//表示用户要传输需要在不安全连接上进行安全连接的安全信息（例如OAuth2令牌）。
	errTransportCredentialsMissing = errors.New("grpc: the credentials require transport level security (use grpc.WithTransportCredentials() to set)")
	//同时调用grpc.WithTransportCredentials（）和grpc.WithInsecure（）。
	errCredentialsConflict = errors.New("grpc: transport credentials are set for an insecure connection (grpc.WithTransportCredentials() and grpc.WithInsecure() are both called)")
)

const (
	//默认的最大的发送和接收信息大小
	defaultClientMaxReceiveMessageSize = 1024 * 1024 * 4//用MaxCallRecvMsgSize 可以改变
	defaultClientMaxSendMessageSize    = math.MaxInt32
	//用于指定 http2IOBufSize 用于发送/读取帧的缓冲区大小。
	defaultWriteBufSize = 32 * 1024
	defaultReadBufSize  = 32 * 1024
)

// Dial 根据 target 开始创建一个客户端连接
func Dial(target string, opts ...DialOption) (*ClientConn, error) {
	return DialContext(context.Background(), target, opts...)
}

type defaultConfigSelector struct {
	sc *ServiceConfig
}

func (dcs *defaultConfigSelector) SelectConfig(rpcInfo iresolver.RPCInfo) (*iresolver.RPCConfig, error) {
	return &iresolver.RPCConfig{
		Context:      rpcInfo.Context,
		MethodConfig: getMethodConfig(dcs.sc, rpcInfo.Method),
	}, nil
}

//创建到给定目标的客户端连接。
//默认情况下，它是非阻塞的（该功能不会等待建立连接，并且连接发生在后台）。要使其成为阻塞的，请使用WithBlock（）。
//在非阻塞情况下，ctx不会对连接起作用。它仅控制设置步骤。
//在阻塞情况下，ctx可用于取消或终止挂起的连接。此函数返回后，ctx的取消和到期将不再起作用。用户应在此函数返回后调用ClientConn.Close终止所有挂起的操作。
//target命名规范参考
//https://github.com/grpc/grpc/blob/master/doc/naming.md.
func DialContext(ctx context.Context, target string, opts ...DialOption) (conn *ClientConn, err error) {
	//ClientConn 对象是连接管理的入口，表示到服务端的一个逻辑的连接，会做名字解析、负载均衡、KeepAlive 等连接管理方面的操作，是个线程安全的对象。
	//每个 ClientConn 对应有多个 SubConn，ClientConn 会基于名字发现（resolver）得到多个 SubConn，并面向多个 SubConn 之间实现负载均衡（balancer）。
	cc := &ClientConn{
		target:            target,
		csMgr:             &connectivityStateManager{},
		conns:             make(map[*addrConn]struct{}),
		dopts:             defaultDialOptions(),
		blockingpicker:    newPickerWrapper(),
		czData:            new(channelzData),
		firstResolveEvent: grpcsync.NewEvent(), //一次性事件
	}
	cc.retryThrottler.Store((*retryThrottler)(nil))
	//这里设置可以取消的 context，调用 cc.cancel() 主动中断 Dial
	cc.ctx, cc.cancel = context.WithCancel(context.Background())

	for _, opt := range opts {
		opt.apply(&cc.dopts) //初始化所有参数
	}

	//这里处理一些埋点操作
	chainUnaryClientInterceptors(cc)  //处理一元拦截器
	chainStreamClientInterceptors(cc) //处理流客户端拦截器

	defer func() {
		if err != nil {
			cc.Close()
		}
	}()

	//设置 channelz，作用是gRPC用来debug问题的一个高阶特性
	//参考文档：
	//https://grpc.io/blog/a-short-introduction-to-channelz/
	//https://github.com/grpc/proposal/blob/master/A14-channelz.md
	if channelz.IsOn() {
		if cc.dopts.channelzParentID != 0 {
			cc.channelzID = channelz.RegisterChannel(&channelzChannel{cc}, cc.dopts.channelzParentID, target)
			channelz.AddTraceEvent(logger, cc.channelzID, 0, &channelz.TraceEventDesc{
				Desc:     "Channel Created",
				Severity: channelz.CtInfo,
				Parent: &channelz.TraceEventDesc{
					Desc:     fmt.Sprintf("Nested Channel(id:%d) created", cc.channelzID),
					Severity: channelz.CtInfo,
				},
			})
		} else {
			cc.channelzID = channelz.RegisterChannel(&channelzChannel{cc}, 0, target)
			channelz.Info(logger, cc.channelzID, "Channel Created")
		}
		cc.csMgr.channelzID = cc.channelzID
	}

	//这里判断与配置ClientConn的传输安全性
	if !cc.dopts.insecure { //通过 WithInsecure 配置
		//表示启用ClientConn的传输安全性时
		if cc.dopts.copts.TransportCredentials == nil && cc.dopts.copts.CredsBundle == nil {
			return nil, errNoTransportSecurity
		}
		if cc.dopts.copts.TransportCredentials != nil && cc.dopts.copts.CredsBundle != nil {
			return nil, errTransportCredsAndBundle
		}
	} else {
		//表示禁用ClientConn的传输安全性时
		if cc.dopts.copts.TransportCredentials != nil || cc.dopts.copts.CredsBundle != nil {
			return nil, errCredentialsConflict
		}
		//单个RPC请求时，还是可以带入验证功能的，所以为了避免认证的问题，建议全部开启认证
		for _, cd := range cc.dopts.copts.PerRPCCredentials {
			if cd.RequireTransportSecurity() {
				return nil, errTransportCredentialsMissing
			}
		}
	}
	//默认的服务配置，以JSON格式输入
	if cc.dopts.defaultServiceConfigRawJSON != nil {
		scpr := parseServiceConfig(*cc.dopts.defaultServiceConfigRawJSON)
		if scpr.Err != nil {
			return nil, fmt.Errorf("%s: %v", invalidDefaultServiceConfigErrPrefix, scpr.Err)
		}
		cc.dopts.defaultServiceConfig, _ = scpr.Config.(*ServiceConfig)
	}

	//配置长连接参数
	cc.mkp = cc.dopts.copts.KeepaliveParams
	//设置 UA
	if cc.dopts.copts.UserAgent != "" {
		cc.dopts.copts.UserAgent += " " + grpcUA
	} else {
		cc.dopts.copts.UserAgent = grpcUA
	}

	if cc.dopts.timeout > 0 { //可以通过 WithTimeout 函数设置
		//这个超时是用作 clientConn 初始化超时判断
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cc.dopts.timeout)
		defer cancel()
	}
	defer func() {
		select {
		case <-ctx.Done(): //收到这个，表示cc.dopts.timeout >0 且已经设置了超时，且 ctx 过期了
			switch {
			case ctx.Err() == err:
				conn = nil
			case err == nil || !cc.dopts.returnLastError:
				conn, err = nil, ctx.Err()
			default:
				conn, err = nil, fmt.Errorf("%v: %v", ctx.Err(), err)
			}
		default:
		}
	}()

	//sc 指的是serviceconfig，已经被弃用
	scSet := false              //是否读取到服务配置标记，true 表示成功获取到，因为 scChan 可以异步设置，因此会出现 scChan 不为 nil，但是 scSet = false的情况
	if cc.dopts.scChan != nil { //通过 WithServiceConfig 设置，是一个具有读取服务配置的通道
		// 尝试获取初始服务配置
		select {
		case sc, ok := <-cc.dopts.scChan:
			if ok {
				cc.sc = &sc
				//应用服务配置
				//注意：这里的更新配置会阻塞的让 ConfigSelector 都应用完成才允许下一次更新
				cc.safeConfigSelector.UpdateConfigSelector(&defaultConfigSelector{&sc})
				scSet = true
			}
		default: //如果在此之前没有设置 scChan，可能会到这里
		}
	}
	if cc.dopts.bs == nil { //用于设置连接尝试失败后，用于connectRetryNum的重试策略，比如重试等待时间，通过 withBackoff 设置
		cc.dopts.bs = backoff.DefaultExponential
	}

	// 这里确定要使用的解析器生成器
	// cc.dopts.copts 涵盖了与服务器通信的所有相关选项
	// cc.dopts.copts.Dialer 指定了如何请求网络上的某个地址
	//这里开始解析 target ，转化成 parsedTarget
	cc.parsedTarget = grpcutil.ParseTarget(cc.target, cc.dopts.copts.Dialer != nil)
	channelz.Infof(logger, cc.channelzID, "parsed scheme: %q", cc.parsedTarget.Scheme) //打印日志，并添加链路追踪
	resolverBuilder := cc.getResolver(cc.parsedTarget.Scheme)                          //可能返回在给定方案中注册的解析器。如果没有在该方案中注册，则将返回nil。
	if resolverBuilder == nil {
		//如果解析程序生成器仍为nil，表明已解析目标的方案未注册。
		//回退到默认解析器，并将 target 设置为 Endpoint。
		channelz.Infof(logger, cc.channelzID, "scheme %q not registered, fallback to default scheme", cc.parsedTarget.Scheme) //打印日志，并添加链路追踪
		cc.parsedTarget = resolver.Target{
			Scheme:   resolver.GetDefaultScheme(),
			Endpoint: target,
		}
		//再次尝试获取解析器
		resolverBuilder = cc.getResolver(cc.parsedTarget.Scheme)
		if resolverBuilder == nil {
			//如果默认的解析器还获取不到，就报错
			return nil, fmt.Errorf("could not get resolver for default scheme: %q", cc.parsedTarget.Scheme)
		}
	}

	//设置认证相关的参数
	//要么是用户指定的服务名，要么是自定义配置中的 authority，要么是解析后的 Endpoint
	creds := cc.dopts.copts.TransportCredentials       //获取建立客户端连接所需要的身份验证
	if creds != nil && creds.Info().ServerName != "" { //如果能获取到身份凭证，且服务名不为空
		cc.authority = creds.Info().ServerName
	} else if cc.dopts.insecure && cc.dopts.authority != "" { //如果自定义配置中设置了传输安全，且设置了 authority
		cc.authority = cc.dopts.authority
	} else if strings.HasPrefix(cc.target, "unix:") || strings.HasPrefix(cc.target, "unix-abstract:") { //判断是否是本地
		cc.authority = "localhost"
	} else if strings.HasPrefix(cc.parsedTarget.Endpoint, ":") {
		cc.authority = "localhost" + cc.parsedTarget.Endpoint
	} else {
		// Use endpoint from "scheme://authority/endpoint" as the default
		// authority for ClientConn.
		cc.authority = cc.parsedTarget.Endpoint
	}
	//相比上面的逻辑，这里是阻塞的等待初始化服务配置
	if cc.dopts.scChan != nil && !scSet {
		// Blocking wait for the initial service config.
		select {
		case sc, ok := <-cc.dopts.scChan:
			if ok {
				cc.sc = &sc
				cc.safeConfigSelector.UpdateConfigSelector(&defaultConfigSelector{&sc})
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if cc.dopts.scChan != nil {
		go cc.scWatcher()//监控ServiceConfig的变化，这样可以在dial之后动态的修改client的访问服务的配置ServiceConfig
	}

	//克隆一份creds
	var credsClone credentials.TransportCredentials
	if creds := cc.dopts.copts.TransportCredentials; creds != nil {
		credsClone = creds.Clone()
	}
	//负载均衡相关参数
	cc.balancerBuildOpts = balancer.BuildOptions{
		DialCreds:        credsClone,
		CredsBundle:      cc.dopts.copts.CredsBundle,
		Dialer:           cc.dopts.copts.Dialer,
		CustomUserAgent:  cc.dopts.copts.UserAgent,
		ChannelzParentID: cc.channelzID,
		Target:           cc.parsedTarget,
	}

	//cc：指的是客户端连接
	//这里会对Resolver做了一层封装
	rWrapper, err := newCCResolverWrapper(cc, resolverBuilder)
	if err != nil {
		return nil, fmt.Errorf("failed to build resolver: %v", err)
	}
	//防止并发
	cc.mu.Lock()
	cc.resolverWrapper = rWrapper
	cc.mu.Unlock()

	//如果设置了 block，那就会在for循环里等待，直到完成连接
	//如果没有设置，就会直接返回
	if cc.dopts.block { //通过 WithBlock（） 设置
		for {
			s := cc.GetState()
			if s == connectivity.Ready {
				break
			} else if cc.dopts.copts.FailOnNonTempDialError && s == connectivity.TransientFailure {
				if err = cc.connectionError(); err != nil {
					terr, ok := err.(interface {
						Temporary() bool
					})
					if ok && !terr.Temporary() {
						return nil, err
					}
				}
			}
			if !cc.WaitForStateChange(ctx, s) {
				// ctx got timeout or canceled.
				if err = cc.connectionError(); err != nil && cc.dopts.returnLastError {
					return nil, err
				}
				return nil, ctx.Err()
			}
		}
	}

	return cc, nil
}

// chainUnaryClientInterceptors chains all unary client interceptors into one.
//将所有一元客户端拦截器都链接到一个上
func chainUnaryClientInterceptors(cc *ClientConn) {
	//获取目前已有的拦截器
	interceptors := cc.dopts.chainUnaryInts
	// Prepend dopts.unaryInt to the chaining interceptors if it exists, since unaryInt will
	// be executed before any other chained interceptors.
	if cc.dopts.unaryInt != nil {
		//将拦截器组装到一起
		interceptors = append([]UnaryClientInterceptor{cc.dopts.unaryInt}, interceptors...)
	}
	var chainedInt UnaryClientInterceptor
	//如果拦截器数量为0，就将一元客户端拦截器置为 nil
	//如果拦截器数量为1，那么一元客户端拦截器置为该拦截器
	//如果拦截器数量大于1，那么取第一个，且将这些拦截器生成一个链接的调用程序
	if len(interceptors) == 0 {
		chainedInt = nil
	} else if len(interceptors) == 1 {
		chainedInt = interceptors[0]
	} else {
		chainedInt = func(ctx context.Context, method string, req, reply interface{}, cc *ClientConn, invoker UnaryInvoker, opts ...CallOption) error {
			return interceptors[0](ctx, method, req, reply, cc, getChainUnaryInvoker(interceptors, 0, invoker), opts...)
		}
	}
	cc.dopts.unaryInt = chainedInt
}

// getChainUnaryInvoker recursively generate the chained unary invoker.
func getChainUnaryInvoker(interceptors []UnaryClientInterceptor, curr int, finalInvoker UnaryInvoker) UnaryInvoker {
	if curr == len(interceptors)-1 {
		return finalInvoker
	}
	return func(ctx context.Context, method string, req, reply interface{}, cc *ClientConn, opts ...CallOption) error {
		return interceptors[curr+1](ctx, method, req, reply, cc, getChainUnaryInvoker(interceptors, curr+1, finalInvoker), opts...)
	}
}

// chainStreamClientInterceptors chains all stream client interceptors into one.
//将所有流客户端拦截器链接到一个上
//逻辑参考 chainUnaryClientInterceptors
func chainStreamClientInterceptors(cc *ClientConn) {
	interceptors := cc.dopts.chainStreamInts
	// Prepend dopts.streamInt to the chaining interceptors if it exists, since streamInt will
	// be executed before any other chained interceptors.
	if cc.dopts.streamInt != nil {
		interceptors = append([]StreamClientInterceptor{cc.dopts.streamInt}, interceptors...)
	}
	var chainedInt StreamClientInterceptor
	if len(interceptors) == 0 {
		chainedInt = nil
	} else if len(interceptors) == 1 {
		chainedInt = interceptors[0]
	} else {
		chainedInt = func(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, streamer Streamer, opts ...CallOption) (ClientStream, error) {
			return interceptors[0](ctx, desc, cc, method, getChainStreamer(interceptors, 0, streamer), opts...)
		}
	}
	cc.dopts.streamInt = chainedInt
}

// getChainStreamer recursively generate the chained client stream constructor.
func getChainStreamer(interceptors []StreamClientInterceptor, curr int, finalStreamer Streamer) Streamer {
	if curr == len(interceptors)-1 {
		return finalStreamer
	}
	return func(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, opts ...CallOption) (ClientStream, error) {
		return interceptors[curr+1](ctx, desc, cc, method, getChainStreamer(interceptors, curr+1, finalStreamer), opts...)
	}
}

// connectivityStateManager keeps the connectivity.State of ClientConn.
// This struct will eventually be exported so the balancers can access it.
type connectivityStateManager struct {
	mu         sync.Mutex
	state      connectivity.State
	notifyChan chan struct{}
	channelzID int64
}

// updateState updates the connectivity.State of ClientConn.
// If there's a change it notifies goroutines waiting on state change to
// happen.
func (csm *connectivityStateManager) updateState(state connectivity.State) {
	csm.mu.Lock()
	defer csm.mu.Unlock()
	if csm.state == connectivity.Shutdown {
		return
	}
	if csm.state == state {
		return
	}
	csm.state = state
	channelz.Infof(logger, csm.channelzID, "Channel Connectivity change to %v", state)
	if csm.notifyChan != nil {
		// There are other goroutines waiting on this channel.
		close(csm.notifyChan)
		csm.notifyChan = nil
	}
}

func (csm *connectivityStateManager) getState() connectivity.State {
	csm.mu.Lock()
	defer csm.mu.Unlock()
	return csm.state
}

func (csm *connectivityStateManager) getNotifyChan() <-chan struct{} {
	csm.mu.Lock()
	defer csm.mu.Unlock()
	if csm.notifyChan == nil {
		csm.notifyChan = make(chan struct{})
	}
	return csm.notifyChan
}

// ClientConnInterface defines the functions clients need to perform unary and
// streaming RPCs.  It is implemented by *ClientConn, and is only intended to
// be referenced by generated code.
type ClientConnInterface interface {
	// Invoke performs a unary RPC and returns after the response is received
	// into reply.
	Invoke(ctx context.Context, method string, args interface{}, reply interface{}, opts ...CallOption) error
	// NewStream begins a streaming RPC.
	NewStream(ctx context.Context, desc *StreamDesc, method string, opts ...CallOption) (ClientStream, error)
}

// Assert *ClientConn implements ClientConnInterface.
var _ ClientConnInterface = (*ClientConn)(nil)

// ClientConn represents a virtual connection to a conceptual endpoint, to
// perform RPCs.
//
// A ClientConn is free to have zero or more actual connections to the endpoint
// based on configuration, load, etc. It is also free to determine which actual
// endpoints to use and may change it every RPC, permitting client-side load
// balancing.
//
// A ClientConn encapsulates a range of functionality including name
// resolution, TCP connection establishment (with retries and backoff) and TLS
// handshakes. It also handles errors on established connections by
// re-resolving the name and reconnecting.
type ClientConn struct {
	ctx    context.Context    //给 dial 进行会话控制
	cancel context.CancelFunc //主动调用cancel，断开 dial

	target       string          //在没有负载均衡的情况下，是服务器的地址：ip+端口；在负载情况下，是etcd的key
	parsedTarget resolver.Target //去解析 target 生成 parsedTarget
	authority    string
	dopts        dialOptions               //建立连接的各种配置
	csMgr        *connectivityStateManager //每个连接的状态管理

	balancerBuildOpts balancer.BuildOptions //与远程负载均衡服务器连接的凭证，若没有安全上的考虑，忽略该项
	blockingpicker    *pickerWrapper

	safeConfigSelector iresolver.SafeConfigSelector

	mu              sync.RWMutex
	resolverWrapper *ccResolverWrapper
	sc              *ServiceConfig
	conns           map[*addrConn]struct{} //维护各个地址对应的长连接
	// Keepalive parameter can be updated if a GoAway is received.
	mkp             keepalive.ClientParameters //针对客户端设置长连接参数
	curBalancerName string
	balancerWrapper *ccBalancerWrapper
	retryThrottler  atomic.Value

	firstResolveEvent *grpcsync.Event

	channelzID int64 // channelz unique identification number
	czData     *channelzData

	lceMu               sync.Mutex // protects lastConnectionError
	lastConnectionError error
}

// WaitForStateChange waits until the connectivity.State of ClientConn changes from sourceState or
// ctx expires. A true value is returned in former case and false in latter.
//
// Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func (cc *ClientConn) WaitForStateChange(ctx context.Context, sourceState connectivity.State) bool {
	ch := cc.csMgr.getNotifyChan()
	if cc.csMgr.getState() != sourceState {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-ch:
		return true
	}
}

// GetState returns the connectivity.State of ClientConn.
//
// Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func (cc *ClientConn) GetState() connectivity.State {
	return cc.csMgr.getState()
}

func (cc *ClientConn) scWatcher() {
	for {
		select {
		case sc, ok := <-cc.dopts.scChan:
			if !ok {
				return
			}
			cc.mu.Lock()
			// TODO: load balance policy runtime change is ignored.
			// We may revisit this decision in the future.
			cc.sc = &sc
			cc.safeConfigSelector.UpdateConfigSelector(&defaultConfigSelector{&sc})
			cc.mu.Unlock()
		case <-cc.ctx.Done():
			return
		}
	}
}

// waitForResolvedAddrs blocks until the resolver has provided addresses or the
// context expires.  Returns nil unless the context expires first; otherwise
// returns a status error based on the context.
func (cc *ClientConn) waitForResolvedAddrs(ctx context.Context) error {
	// This is on the RPC path, so we use a fast path to avoid the
	// more-expensive "select" below after the resolver has returned once.
	if cc.firstResolveEvent.HasFired() {
		return nil
	}
	select {
	case <-cc.firstResolveEvent.Done():
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-cc.ctx.Done():
		return ErrClientConnClosing
	}
}

var emptyServiceConfig *ServiceConfig

func init() {
	cfg := parseServiceConfig("{}")
	if cfg.Err != nil {
		panic(fmt.Sprintf("impossible error parsing empty service config: %v", cfg.Err))
	}
	emptyServiceConfig = cfg.Config.(*ServiceConfig)
}

func (cc *ClientConn) maybeApplyDefaultServiceConfig(addrs []resolver.Address) {
	if cc.sc != nil {
		cc.applyServiceConfigAndBalancer(cc.sc, nil, addrs)
		return
	}
	if cc.dopts.defaultServiceConfig != nil {
		cc.applyServiceConfigAndBalancer(cc.dopts.defaultServiceConfig, &defaultConfigSelector{cc.dopts.defaultServiceConfig}, addrs)
	} else {
		cc.applyServiceConfigAndBalancer(emptyServiceConfig, &defaultConfigSelector{emptyServiceConfig}, addrs)
	}
}

func (cc *ClientConn) updateResolverState(s resolver.State, err error) error {
	defer cc.firstResolveEvent.Fire()
	cc.mu.Lock()
	// Check if the ClientConn is already closed. Some fields (e.g.
	// balancerWrapper) are set to nil when closing the ClientConn, and could
	// cause nil pointer panic if we don't have this check.
	if cc.conns == nil {
		cc.mu.Unlock()
		return nil
	}

	if err != nil {
		// May need to apply the initial service config in case the resolver
		// doesn't support service configs, or doesn't provide a service config
		// with the new addresses.
		cc.maybeApplyDefaultServiceConfig(nil)

		if cc.balancerWrapper != nil {
			cc.balancerWrapper.resolverError(err)
		}

		// No addresses are valid with err set; return early.
		cc.mu.Unlock()
		return balancer.ErrBadResolverState
	}

	var ret error
	if cc.dopts.disableServiceConfig || s.ServiceConfig == nil {
		cc.maybeApplyDefaultServiceConfig(s.Addresses)
		// TODO: do we need to apply a failing LB policy if there is no
		// default, per the error handling design?
	} else {
		if sc, ok := s.ServiceConfig.Config.(*ServiceConfig); s.ServiceConfig.Err == nil && ok {
			configSelector := iresolver.GetConfigSelector(s)
			if configSelector != nil {
				if len(s.ServiceConfig.Config.(*ServiceConfig).Methods) != 0 {
					channelz.Infof(logger, cc.channelzID, "method configs in service config will be ignored due to presence of config selector")
				}
			} else {
				configSelector = &defaultConfigSelector{sc}
			}
			cc.applyServiceConfigAndBalancer(sc, configSelector, s.Addresses)
		} else {
			ret = balancer.ErrBadResolverState
			if cc.balancerWrapper == nil {
				var err error
				if s.ServiceConfig.Err != nil {
					err = status.Errorf(codes.Unavailable, "error parsing service config: %v", s.ServiceConfig.Err)
				} else {
					err = status.Errorf(codes.Unavailable, "illegal service config type: %T", s.ServiceConfig.Config)
				}
				cc.safeConfigSelector.UpdateConfigSelector(&defaultConfigSelector{cc.sc})
				cc.blockingpicker.updatePicker(base.NewErrPicker(err))
				cc.csMgr.updateState(connectivity.TransientFailure)
				cc.mu.Unlock()
				return ret
			}
		}
	}

	var balCfg serviceconfig.LoadBalancingConfig
	if cc.dopts.balancerBuilder == nil && cc.sc != nil && cc.sc.lbConfig != nil {
		balCfg = cc.sc.lbConfig.cfg
	}

	cbn := cc.curBalancerName
	bw := cc.balancerWrapper
	cc.mu.Unlock()
	if cbn != grpclbName {
		// Filter any grpclb addresses since we don't have the grpclb balancer.
		for i := 0; i < len(s.Addresses); {
			if s.Addresses[i].Type == resolver.GRPCLB {
				copy(s.Addresses[i:], s.Addresses[i+1:])
				s.Addresses = s.Addresses[:len(s.Addresses)-1]
				continue
			}
			i++
		}
	}
	uccsErr := bw.updateClientConnState(&balancer.ClientConnState{ResolverState: s, BalancerConfig: balCfg})
	if ret == nil {
		ret = uccsErr // prefer ErrBadResolver state since any other error is
		// currently meaningless to the caller.
	}
	return ret
}

// switchBalancer starts the switching from current balancer to the balancer
// with the given name.
//
// It will NOT send the current address list to the new balancer. If needed,
// caller of this function should send address list to the new balancer after
// this function returns.
//
// Caller must hold cc.mu.
func (cc *ClientConn) switchBalancer(name string) {
	if strings.EqualFold(cc.curBalancerName, name) {
		return
	}

	channelz.Infof(logger, cc.channelzID, "ClientConn switching balancer to %q", name)
	if cc.dopts.balancerBuilder != nil {
		channelz.Info(logger, cc.channelzID, "ignoring balancer switching: Balancer DialOption used instead")
		return
	}
	if cc.balancerWrapper != nil {
		cc.balancerWrapper.close()
	}

	builder := balancer.Get(name)
	if builder == nil {
		channelz.Warningf(logger, cc.channelzID, "Channel switches to new LB policy %q due to fallback from invalid balancer name", PickFirstBalancerName)
		channelz.Infof(logger, cc.channelzID, "failed to get balancer builder for: %v, using pick_first instead", name)
		builder = newPickfirstBuilder()
	} else {
		channelz.Infof(logger, cc.channelzID, "Channel switches to new LB policy %q", name)
	}

	cc.curBalancerName = builder.Name()
	cc.balancerWrapper = newCCBalancerWrapper(cc, builder, cc.balancerBuildOpts)
}

func (cc *ClientConn) handleSubConnStateChange(sc balancer.SubConn, s connectivity.State, err error) {
	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return
	}
	// TODO(bar switching) send updates to all balancer wrappers when balancer
	// gracefully switching is supported.
	cc.balancerWrapper.handleSubConnStateChange(sc, s, err)
	cc.mu.Unlock()
}

// newAddrConn creates an addrConn for addrs and adds it to cc.conns.
//
// Caller needs to make sure len(addrs) > 0.
func (cc *ClientConn) newAddrConn(addrs []resolver.Address, opts balancer.NewSubConnOptions) (*addrConn, error) {
	ac := &addrConn{
		state:        connectivity.Idle,
		cc:           cc,
		addrs:        addrs,
		scopts:       opts,
		dopts:        cc.dopts,
		czData:       new(channelzData),
		resetBackoff: make(chan struct{}),
	}
	ac.ctx, ac.cancel = context.WithCancel(cc.ctx)
	// Track ac in cc. This needs to be done before any getTransport(...) is called.
	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return nil, ErrClientConnClosing
	}
	if channelz.IsOn() {
		ac.channelzID = channelz.RegisterSubChannel(ac, cc.channelzID, "")
		channelz.AddTraceEvent(logger, ac.channelzID, 0, &channelz.TraceEventDesc{
			Desc:     "Subchannel Created",
			Severity: channelz.CtInfo,
			Parent: &channelz.TraceEventDesc{
				Desc:     fmt.Sprintf("Subchannel(id:%d) created", ac.channelzID),
				Severity: channelz.CtInfo,
			},
		})
	}
	cc.conns[ac] = struct{}{}
	cc.mu.Unlock()
	return ac, nil
}

// removeAddrConn removes the addrConn in the subConn from clientConn.
// It also tears down the ac with the given error.
func (cc *ClientConn) removeAddrConn(ac *addrConn, err error) {
	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return
	}
	delete(cc.conns, ac)
	cc.mu.Unlock()
	ac.tearDown(err)
}

func (cc *ClientConn) channelzMetric() *channelz.ChannelInternalMetric {
	return &channelz.ChannelInternalMetric{
		State:                    cc.GetState(),
		Target:                   cc.target,
		CallsStarted:             atomic.LoadInt64(&cc.czData.callsStarted),
		CallsSucceeded:           atomic.LoadInt64(&cc.czData.callsSucceeded),
		CallsFailed:              atomic.LoadInt64(&cc.czData.callsFailed),
		LastCallStartedTimestamp: time.Unix(0, atomic.LoadInt64(&cc.czData.lastCallStartedTime)),
	}
}

// Target returns the target string of the ClientConn.
//
// Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func (cc *ClientConn) Target() string {
	return cc.target
}

func (cc *ClientConn) incrCallsStarted() {
	atomic.AddInt64(&cc.czData.callsStarted, 1)
	atomic.StoreInt64(&cc.czData.lastCallStartedTime, time.Now().UnixNano())
}

func (cc *ClientConn) incrCallsSucceeded() {
	atomic.AddInt64(&cc.czData.callsSucceeded, 1)
}

func (cc *ClientConn) incrCallsFailed() {
	atomic.AddInt64(&cc.czData.callsFailed, 1)
}

// connect starts creating a transport.
// It does nothing if the ac is not IDLE.
// TODO(bar) Move this to the addrConn section.
func (ac *addrConn) connect() error {
	ac.mu.Lock()
	if ac.state == connectivity.Shutdown {
		ac.mu.Unlock()
		return errConnClosing
	}
	if ac.state != connectivity.Idle {
		ac.mu.Unlock()
		return nil
	}
	// Update connectivity state within the lock to prevent subsequent or
	// concurrent calls from resetting the transport more than once.
	ac.updateConnectivityState(connectivity.Connecting, nil)
	ac.mu.Unlock()

	// Start a goroutine connecting to the server asynchronously.
	go ac.resetTransport()
	return nil
}

// tryUpdateAddrs tries to update ac.addrs with the new addresses list.
//
// If ac is Connecting, it returns false. The caller should tear down the ac and
// create a new one. Note that the backoff will be reset when this happens.
//
// If ac is TransientFailure, it updates ac.addrs and returns true. The updated
// addresses will be picked up by retry in the next iteration after backoff.
//
// If ac is Shutdown or Idle, it updates ac.addrs and returns true.
//
// If ac is Ready, it checks whether current connected address of ac is in the
// new addrs list.
//  - If true, it updates ac.addrs and returns true. The ac will keep using
//    the existing connection.
//  - If false, it does nothing and returns false.
func (ac *addrConn) tryUpdateAddrs(addrs []resolver.Address) bool {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	channelz.Infof(logger, ac.channelzID, "addrConn: tryUpdateAddrs curAddr: %v, addrs: %v", ac.curAddr, addrs)
	if ac.state == connectivity.Shutdown ||
		ac.state == connectivity.TransientFailure ||
		ac.state == connectivity.Idle {
		ac.addrs = addrs
		return true
	}

	if ac.state == connectivity.Connecting {
		return false
	}

	// ac.state is Ready, try to find the connected address.
	var curAddrFound bool
	for _, a := range addrs {
		if reflect.DeepEqual(ac.curAddr, a) {
			curAddrFound = true
			break
		}
	}
	channelz.Infof(logger, ac.channelzID, "addrConn: tryUpdateAddrs curAddrFound: %v", curAddrFound)
	if curAddrFound {
		ac.addrs = addrs
	}

	return curAddrFound
}

func getMethodConfig(sc *ServiceConfig, method string) MethodConfig {
	if sc == nil {
		return MethodConfig{}
	}
	if m, ok := sc.Methods[method]; ok {
		return m
	}
	i := strings.LastIndex(method, "/")
	if m, ok := sc.Methods[method[:i+1]]; ok {
		return m
	}
	return sc.Methods[""]
}

// GetMethodConfig gets the method config of the input method.
// If there's an exact match for input method (i.e. /service/method), we return
// the corresponding MethodConfig.
// If there isn't an exact match for the input method, we look for the service's default
// config under the service (i.e /service/) and then for the default for all services (empty string).
//
// If there is a default MethodConfig for the service, we return it.
// Otherwise, we return an empty MethodConfig.
func (cc *ClientConn) GetMethodConfig(method string) MethodConfig {
	// TODO: Avoid the locking here.
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	return getMethodConfig(cc.sc, method)
}

func (cc *ClientConn) healthCheckConfig() *healthCheckConfig {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if cc.sc == nil {
		return nil
	}
	return cc.sc.healthCheckConfig
}

func (cc *ClientConn) getTransport(ctx context.Context, failfast bool, method string) (transport.ClientTransport, func(balancer.DoneInfo), error) {
	t, done, err := cc.blockingpicker.pick(ctx, failfast, balancer.PickInfo{
		Ctx:            ctx,
		FullMethodName: method,
	})
	if err != nil {
		return nil, nil, toRPCErr(err)
	}
	return t, done, nil
}

func (cc *ClientConn) applyServiceConfigAndBalancer(sc *ServiceConfig, configSelector iresolver.ConfigSelector, addrs []resolver.Address) {
	if sc == nil {
		// should never reach here.
		return
	}
	cc.sc = sc
	if configSelector != nil {
		cc.safeConfigSelector.UpdateConfigSelector(configSelector)
	}

	if cc.sc.retryThrottling != nil {
		newThrottler := &retryThrottler{
			tokens: cc.sc.retryThrottling.MaxTokens,
			max:    cc.sc.retryThrottling.MaxTokens,
			thresh: cc.sc.retryThrottling.MaxTokens / 2,
			ratio:  cc.sc.retryThrottling.TokenRatio,
		}
		cc.retryThrottler.Store(newThrottler)
	} else {
		cc.retryThrottler.Store((*retryThrottler)(nil))
	}

	if cc.dopts.balancerBuilder == nil {
		// Only look at balancer types and switch balancer if balancer dial
		// option is not set.
		var newBalancerName string
		if cc.sc != nil && cc.sc.lbConfig != nil {
			newBalancerName = cc.sc.lbConfig.name
		} else {
			var isGRPCLB bool
			for _, a := range addrs {
				if a.Type == resolver.GRPCLB {
					isGRPCLB = true
					break
				}
			}
			if isGRPCLB {
				newBalancerName = grpclbName
			} else if cc.sc != nil && cc.sc.LB != nil {
				newBalancerName = *cc.sc.LB
			} else {
				newBalancerName = PickFirstBalancerName
			}
		}
		cc.switchBalancer(newBalancerName)
	} else if cc.balancerWrapper == nil {
		// Balancer dial option was set, and this is the first time handling
		// resolved addresses. Build a balancer with dopts.balancerBuilder.
		cc.curBalancerName = cc.dopts.balancerBuilder.Name()
		cc.balancerWrapper = newCCBalancerWrapper(cc, cc.dopts.balancerBuilder, cc.balancerBuildOpts)
	}
}

func (cc *ClientConn) resolveNow(o resolver.ResolveNowOptions) {
	cc.mu.RLock()
	r := cc.resolverWrapper
	cc.mu.RUnlock()
	if r == nil {
		return
	}
	go r.resolveNow(o)
}

// ResetConnectBackoff wakes up all subchannels in transient failure and causes
// them to attempt another connection immediately.  It also resets the backoff
// times used for subsequent attempts regardless of the current state.
//
// In general, this function should not be used.  Typical service or network
// outages result in a reasonable client reconnection strategy by default.
// However, if a previously unavailable network becomes available, this may be
// used to trigger an immediate reconnect.
//
// Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func (cc *ClientConn) ResetConnectBackoff() {
	cc.mu.Lock()
	conns := cc.conns
	cc.mu.Unlock()
	for ac := range conns {
		ac.resetConnectBackoff()
	}
}

// Close tears down the ClientConn and all underlying connections.
func (cc *ClientConn) Close() error {
	defer cc.cancel()

	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return ErrClientConnClosing
	}
	conns := cc.conns
	cc.conns = nil
	cc.csMgr.updateState(connectivity.Shutdown)

	rWrapper := cc.resolverWrapper
	cc.resolverWrapper = nil
	bWrapper := cc.balancerWrapper
	cc.balancerWrapper = nil
	cc.mu.Unlock()

	cc.blockingpicker.close()

	if rWrapper != nil {
		rWrapper.close()
	}
	if bWrapper != nil {
		bWrapper.close()
	}

	for ac := range conns {
		ac.tearDown(ErrClientConnClosing)
	}
	if channelz.IsOn() {
		ted := &channelz.TraceEventDesc{
			Desc:     "Channel Deleted",
			Severity: channelz.CtInfo,
		}
		if cc.dopts.channelzParentID != 0 {
			ted.Parent = &channelz.TraceEventDesc{
				Desc:     fmt.Sprintf("Nested channel(id:%d) deleted", cc.channelzID),
				Severity: channelz.CtInfo,
			}
		}
		channelz.AddTraceEvent(logger, cc.channelzID, 0, ted)
		// TraceEvent needs to be called before RemoveEntry, as TraceEvent may add trace reference to
		// the entity being deleted, and thus prevent it from being deleted right away.
		channelz.RemoveEntry(cc.channelzID)
	}
	return nil
}

// addrConn is a network connection to a given address.
type addrConn struct {
	ctx    context.Context
	cancel context.CancelFunc

	cc     *ClientConn
	dopts  dialOptions
	acbw   balancer.SubConn
	scopts balancer.NewSubConnOptions

	// transport is set when there's a viable transport (note: ac state may not be READY as LB channel
	// health checking may require server to report healthy to set ac to READY), and is reset
	// to nil when the current transport should no longer be used to create a stream (e.g. after GoAway
	// is received, transport is closed, ac has been torn down).
	transport transport.ClientTransport // The current transport.

	mu      sync.Mutex
	curAddr resolver.Address   // The current address.
	addrs   []resolver.Address // All addresses that the resolver resolved to.

	// Use updateConnectivityState for updating addrConn's connectivity state.
	state connectivity.State

	backoffIdx   int // Needs to be stateful for resetConnectBackoff.
	resetBackoff chan struct{}

	channelzID int64 // channelz unique identification number.
	czData     *channelzData
}

// Note: this requires a lock on ac.mu.
func (ac *addrConn) updateConnectivityState(s connectivity.State, lastErr error) {
	if ac.state == s {
		return
	}
	ac.state = s
	channelz.Infof(logger, ac.channelzID, "Subchannel Connectivity change to %v", s)
	ac.cc.handleSubConnStateChange(ac.acbw, s, lastErr)
}

// adjustParams updates parameters used to create transports upon
// receiving a GoAway.
func (ac *addrConn) adjustParams(r transport.GoAwayReason) {
	switch r {
	case transport.GoAwayTooManyPings:
		v := 2 * ac.dopts.copts.KeepaliveParams.Time
		ac.cc.mu.Lock()
		if v > ac.cc.mkp.Time {
			ac.cc.mkp.Time = v
		}
		ac.cc.mu.Unlock()
	}
}

func (ac *addrConn) resetTransport() {
	for i := 0; ; i++ {
		if i > 0 {
			ac.cc.resolveNow(resolver.ResolveNowOptions{})
		}

		ac.mu.Lock()
		if ac.state == connectivity.Shutdown {
			ac.mu.Unlock()
			return
		}

		addrs := ac.addrs
		backoffFor := ac.dopts.bs.Backoff(ac.backoffIdx)
		// This will be the duration that dial gets to finish.
		dialDuration := minConnectTimeout
		if ac.dopts.minConnectTimeout != nil {
			dialDuration = ac.dopts.minConnectTimeout()
		}

		if dialDuration < backoffFor {
			// Give dial more time as we keep failing to connect.
			dialDuration = backoffFor
		}
		// We can potentially spend all the time trying the first address, and
		// if the server accepts the connection and then hangs, the following
		// addresses will never be tried.
		//
		// The spec doesn't mention what should be done for multiple addresses.
		// https://github.com/grpc/grpc/blob/master/doc/connection-backoff.md#proposed-backoff-algorithm
		connectDeadline := time.Now().Add(dialDuration)

		ac.updateConnectivityState(connectivity.Connecting, nil)
		ac.transport = nil
		ac.mu.Unlock()

		newTr, addr, reconnect, err := ac.tryAllAddrs(addrs, connectDeadline)
		if err != nil {
			// After exhausting all addresses, the addrConn enters
			// TRANSIENT_FAILURE.
			ac.mu.Lock()
			if ac.state == connectivity.Shutdown {
				ac.mu.Unlock()
				return
			}
			ac.updateConnectivityState(connectivity.TransientFailure, err)

			// Backoff.
			b := ac.resetBackoff
			ac.mu.Unlock()

			timer := time.NewTimer(backoffFor)
			select {
			case <-timer.C:
				ac.mu.Lock()
				ac.backoffIdx++
				ac.mu.Unlock()
			case <-b:
				timer.Stop()
			case <-ac.ctx.Done():
				timer.Stop()
				return
			}
			continue
		}

		ac.mu.Lock()
		if ac.state == connectivity.Shutdown {
			ac.mu.Unlock()
			newTr.Close()
			return
		}
		ac.curAddr = addr
		ac.transport = newTr
		ac.backoffIdx = 0

		hctx, hcancel := context.WithCancel(ac.ctx)
		ac.startHealthCheck(hctx)
		ac.mu.Unlock()

		// Block until the created transport is down. And when this happens,
		// we restart from the top of the addr list.
		<-reconnect.Done()
		hcancel()
		// restart connecting - the top of the loop will set state to
		// CONNECTING.  This is against the current connectivity semantics doc,
		// however it allows for graceful behavior for RPCs not yet dispatched
		// - unfortunate timing would otherwise lead to the RPC failing even
		// though the TRANSIENT_FAILURE state (called for by the doc) would be
		// instantaneous.
		//
		// Ideally we should transition to Idle here and block until there is
		// RPC activity that leads to the balancer requesting a reconnect of
		// the associated SubConn.
	}
}

// tryAllAddrs tries to creates a connection to the addresses, and stop when at the
// first successful one. It returns the transport, the address and a Event in
// the successful case. The Event fires when the returned transport disconnects.
func (ac *addrConn) tryAllAddrs(addrs []resolver.Address, connectDeadline time.Time) (transport.ClientTransport, resolver.Address, *grpcsync.Event, error) {
	var firstConnErr error
	for _, addr := range addrs {
		ac.mu.Lock()
		if ac.state == connectivity.Shutdown {
			ac.mu.Unlock()
			return nil, resolver.Address{}, nil, errConnClosing
		}

		ac.cc.mu.RLock()
		ac.dopts.copts.KeepaliveParams = ac.cc.mkp
		ac.cc.mu.RUnlock()

		copts := ac.dopts.copts
		if ac.scopts.CredsBundle != nil {
			copts.CredsBundle = ac.scopts.CredsBundle
		}
		ac.mu.Unlock()

		channelz.Infof(logger, ac.channelzID, "Subchannel picks a new address %q to connect", addr.Addr)

		newTr, reconnect, err := ac.createTransport(addr, copts, connectDeadline)
		if err == nil {
			return newTr, addr, reconnect, nil
		}
		if firstConnErr == nil {
			firstConnErr = err
		}
		ac.cc.updateConnectionError(err)
	}

	// Couldn't connect to any address.
	return nil, resolver.Address{}, nil, firstConnErr
}

// createTransport creates a connection to addr. It returns the transport and a
// Event in the successful case. The Event fires when the returned transport
// disconnects.
func (ac *addrConn) createTransport(addr resolver.Address, copts transport.ConnectOptions, connectDeadline time.Time) (transport.ClientTransport, *grpcsync.Event, error) {
	prefaceReceived := make(chan struct{})
	onCloseCalled := make(chan struct{})
	reconnect := grpcsync.NewEvent()

	// addr.ServerName takes precedent over ClientConn authority, if present.
	if addr.ServerName == "" {
		addr.ServerName = ac.cc.authority
	}

	once := sync.Once{}
	onGoAway := func(r transport.GoAwayReason) {
		ac.mu.Lock()
		ac.adjustParams(r)
		once.Do(func() {
			if ac.state == connectivity.Ready {
				// Prevent this SubConn from being used for new RPCs by setting its
				// state to Connecting.
				//
				// TODO: this should be Idle when grpc-go properly supports it.
				ac.updateConnectivityState(connectivity.Connecting, nil)
			}
		})
		ac.mu.Unlock()
		reconnect.Fire()
	}

	onClose := func() {
		ac.mu.Lock()
		once.Do(func() {
			if ac.state == connectivity.Ready {
				// Prevent this SubConn from being used for new RPCs by setting its
				// state to Connecting.
				//
				// TODO: this should be Idle when grpc-go properly supports it.
				ac.updateConnectivityState(connectivity.Connecting, nil)
			}
		})
		ac.mu.Unlock()
		close(onCloseCalled)
		reconnect.Fire()
	}

	onPrefaceReceipt := func() {
		close(prefaceReceived)
	}

	connectCtx, cancel := context.WithDeadline(ac.ctx, connectDeadline)
	defer cancel()
	if channelz.IsOn() {
		copts.ChannelzParentID = ac.channelzID
	}

	newTr, err := transport.NewClientTransport(connectCtx, ac.cc.ctx, addr, copts, onPrefaceReceipt, onGoAway, onClose)
	if err != nil {
		// newTr is either nil, or closed.
		channelz.Warningf(logger, ac.channelzID, "grpc: addrConn.createTransport failed to connect to %v. Err: %v. Reconnecting...", addr, err)
		return nil, nil, err
	}

	select {
	case <-time.After(time.Until(connectDeadline)):
		// We didn't get the preface in time.
		newTr.Close()
		channelz.Warningf(logger, ac.channelzID, "grpc: addrConn.createTransport failed to connect to %v: didn't receive server preface in time. Reconnecting...", addr)
		return nil, nil, errors.New("timed out waiting for server handshake")
	case <-prefaceReceived:
		// We got the preface - huzzah! things are good.
	case <-onCloseCalled:
		// The transport has already closed - noop.
		return nil, nil, errors.New("connection closed")
		// TODO(deklerk) this should bail on ac.ctx.Done(). Add a test and fix.
	}
	return newTr, reconnect, nil
}

// startHealthCheck starts the health checking stream (RPC) to watch the health
// stats of this connection if health checking is requested and configured.
//
// LB channel health checking is enabled when all requirements below are met:
// 1. it is not disabled by the user with the WithDisableHealthCheck DialOption
// 2. internal.HealthCheckFunc is set by importing the grpc/health package
// 3. a service config with non-empty healthCheckConfig field is provided
// 4. the load balancer requests it
//
// It sets addrConn to READY if the health checking stream is not started.
//
// Caller must hold ac.mu.
func (ac *addrConn) startHealthCheck(ctx context.Context) {
	var healthcheckManagingState bool
	defer func() {
		if !healthcheckManagingState {
			ac.updateConnectivityState(connectivity.Ready, nil)
		}
	}()

	if ac.cc.dopts.disableHealthCheck {
		return
	}
	healthCheckConfig := ac.cc.healthCheckConfig()
	if healthCheckConfig == nil {
		return
	}
	if !ac.scopts.HealthCheckEnabled {
		return
	}
	healthCheckFunc := ac.cc.dopts.healthCheckFunc
	if healthCheckFunc == nil {
		// The health package is not imported to set health check function.
		//
		// TODO: add a link to the health check doc in the error message.
		channelz.Error(logger, ac.channelzID, "Health check is requested but health check function is not set.")
		return
	}

	healthcheckManagingState = true

	// Set up the health check helper functions.
	currentTr := ac.transport
	newStream := func(method string) (interface{}, error) {
		ac.mu.Lock()
		if ac.transport != currentTr {
			ac.mu.Unlock()
			return nil, status.Error(codes.Canceled, "the provided transport is no longer valid to use")
		}
		ac.mu.Unlock()
		return newNonRetryClientStream(ctx, &StreamDesc{ServerStreams: true}, method, currentTr, ac)
	}
	setConnectivityState := func(s connectivity.State, lastErr error) {
		ac.mu.Lock()
		defer ac.mu.Unlock()
		if ac.transport != currentTr {
			return
		}
		ac.updateConnectivityState(s, lastErr)
	}
	// Start the health checking stream.
	go func() {
		err := ac.cc.dopts.healthCheckFunc(ctx, newStream, setConnectivityState, healthCheckConfig.ServiceName)
		if err != nil {
			if status.Code(err) == codes.Unimplemented {
				channelz.Error(logger, ac.channelzID, "Subchannel health check is unimplemented at server side, thus health check is disabled")
			} else {
				channelz.Errorf(logger, ac.channelzID, "HealthCheckFunc exits with unexpected error %v", err)
			}
		}
	}()
}

func (ac *addrConn) resetConnectBackoff() {
	ac.mu.Lock()
	close(ac.resetBackoff)
	ac.backoffIdx = 0
	ac.resetBackoff = make(chan struct{})
	ac.mu.Unlock()
}

// getReadyTransport returns the transport if ac's state is READY.
// Otherwise it returns nil, false.
// If ac's state is IDLE, it will trigger ac to connect.
func (ac *addrConn) getReadyTransport() (transport.ClientTransport, bool) {
	ac.mu.Lock()
	if ac.state == connectivity.Ready && ac.transport != nil {
		t := ac.transport
		ac.mu.Unlock()
		return t, true
	}
	var idle bool
	if ac.state == connectivity.Idle {
		idle = true
	}
	ac.mu.Unlock()
	// Trigger idle ac to connect.
	if idle {
		ac.connect()
	}
	return nil, false
}

// tearDown starts to tear down the addrConn.
// TODO(zhaoq): Make this synchronous to avoid unbounded memory consumption in
// some edge cases (e.g., the caller opens and closes many addrConn's in a
// tight loop.
// tearDown doesn't remove ac from ac.cc.conns.
func (ac *addrConn) tearDown(err error) {
	ac.mu.Lock()
	if ac.state == connectivity.Shutdown {
		ac.mu.Unlock()
		return
	}
	curTr := ac.transport
	ac.transport = nil
	// We have to set the state to Shutdown before anything else to prevent races
	// between setting the state and logic that waits on context cancellation / etc.
	ac.updateConnectivityState(connectivity.Shutdown, nil)
	ac.cancel()
	ac.curAddr = resolver.Address{}
	if err == errConnDrain && curTr != nil {
		// GracefulClose(...) may be executed multiple times when
		// i) receiving multiple GoAway frames from the server; or
		// ii) there are concurrent name resolver/Balancer triggered
		// address removal and GoAway.
		// We have to unlock and re-lock here because GracefulClose => Close => onClose, which requires locking ac.mu.
		ac.mu.Unlock()
		curTr.GracefulClose()
		ac.mu.Lock()
	}
	if channelz.IsOn() {
		channelz.AddTraceEvent(logger, ac.channelzID, 0, &channelz.TraceEventDesc{
			Desc:     "Subchannel Deleted",
			Severity: channelz.CtInfo,
			Parent: &channelz.TraceEventDesc{
				Desc:     fmt.Sprintf("Subchanel(id:%d) deleted", ac.channelzID),
				Severity: channelz.CtInfo,
			},
		})
		// TraceEvent needs to be called before RemoveEntry, as TraceEvent may add trace reference to
		// the entity being deleted, and thus prevent it from being deleted right away.
		channelz.RemoveEntry(ac.channelzID)
	}
	ac.mu.Unlock()
}

func (ac *addrConn) getState() connectivity.State {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.state
}

func (ac *addrConn) ChannelzMetric() *channelz.ChannelInternalMetric {
	ac.mu.Lock()
	addr := ac.curAddr.Addr
	ac.mu.Unlock()
	return &channelz.ChannelInternalMetric{
		State:                    ac.getState(),
		Target:                   addr,
		CallsStarted:             atomic.LoadInt64(&ac.czData.callsStarted),
		CallsSucceeded:           atomic.LoadInt64(&ac.czData.callsSucceeded),
		CallsFailed:              atomic.LoadInt64(&ac.czData.callsFailed),
		LastCallStartedTimestamp: time.Unix(0, atomic.LoadInt64(&ac.czData.lastCallStartedTime)),
	}
}

func (ac *addrConn) incrCallsStarted() {
	atomic.AddInt64(&ac.czData.callsStarted, 1)
	atomic.StoreInt64(&ac.czData.lastCallStartedTime, time.Now().UnixNano())
}

func (ac *addrConn) incrCallsSucceeded() {
	atomic.AddInt64(&ac.czData.callsSucceeded, 1)
}

func (ac *addrConn) incrCallsFailed() {
	atomic.AddInt64(&ac.czData.callsFailed, 1)
}

type retryThrottler struct {
	max    float64
	thresh float64
	ratio  float64

	mu     sync.Mutex
	tokens float64 // TODO(dfawley): replace with atomic and remove lock.
}

// throttle subtracts a retry token from the pool and returns whether a retry
// should be throttled (disallowed) based upon the retry throttling policy in
// the service config.
func (rt *retryThrottler) throttle() bool {
	if rt == nil {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tokens--
	if rt.tokens < 0 {
		rt.tokens = 0
	}
	return rt.tokens <= rt.thresh
}

func (rt *retryThrottler) successfulRPC() {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tokens += rt.ratio
	if rt.tokens > rt.max {
		rt.tokens = rt.max
	}
}

type channelzChannel struct {
	cc *ClientConn
}

func (c *channelzChannel) ChannelzMetric() *channelz.ChannelInternalMetric {
	return c.cc.channelzMetric()
}

// ErrClientConnTimeout indicates that the ClientConn cannot establish the
// underlying connections within the specified timeout.
//
// Deprecated: This error is never returned by grpc and should not be
// referenced by users.
var ErrClientConnTimeout = errors.New("grpc: timed out when dialing")

func (cc *ClientConn) getResolver(scheme string) resolver.Builder {
	for _, rb := range cc.dopts.resolvers {
		if scheme == rb.Scheme() {
			return rb
		}
	}
	return resolver.Get(scheme)
}

func (cc *ClientConn) updateConnectionError(err error) {
	cc.lceMu.Lock()
	cc.lastConnectionError = err
	cc.lceMu.Unlock()
}

func (cc *ClientConn) connectionError() error {
	cc.lceMu.Lock()
	defer cc.lceMu.Unlock()
	return cc.lastConnectionError
}