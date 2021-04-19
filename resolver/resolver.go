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


//该包是专门用作 grpc名词解析使用
package resolver

import (
	"context"
	"net"

	"google.golang.org/grpc/attributes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/serviceconfig"
)

var (
	//k:scheme v:对应的解析器生成器
	m = make(map[string]Builder)
	//默认的 scheme
	defaultScheme = "passthrough"
)

// TODO(bar) install dns resolver in init(){}.

//注册 scheme 和对应的解析器生成器到 map中
//注意：此函数只能在初始化期间调用（即在init（）函数中），并且不是线程安全的。如果使用相同的名称注册了多个解析器，则最后一个注册的解析器将生效。
func Register(b Builder) {
	m[b.Scheme()] = b
}

//通过一个 scheme 获取一个解析器生成器
//如果获取不到，就返回为 nil
func Get(scheme string) Builder {
	if b, ok := m[scheme]; ok {
		return b
	}
	return nil
}

//注意：此函数只能在初始化期间调用（即在init（）函数中），并且不是线程安全的。最后设置的方案将覆盖先前设置的值。
func SetDefaultScheme(scheme string) {
	defaultScheme = scheme
}

// GetDefaultScheme gets the default scheme that will be used.
func GetDefaultScheme() string {
	return defaultScheme
}

// AddressType 表示通过名称解析返回的地址类型。
//
// Deprecated: use Attributes in Address instead.
type AddressType uint8

const (
	// Backend indicates the address is for a backend server.
	//
	// Deprecated: use Attributes in Address instead.
	Backend AddressType = iota
	// GRPCLB indicates the address is for a grpclb load balancer.
	//
	// Deprecated: to select the GRPCLB load balancing policy, use a service
	// config with a corresponding loadBalancingConfig.  To supply balancer
	// addresses to the GRPCLB load balancing policy, set State.Attributes
	// using balancer/grpclb/state.Set.
	GRPCLB
)

// Address 代表客户端连接到的服务器
//
// Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type Address struct {
	// Addr is the server address on which a connection will be established.
	Addr string

	// ServerName is the name of this address.
	// If non-empty, the ServerName is used as the transport certification authority for
	// the address, instead of the hostname from the Dial target string. In most cases,
	// this should not be set.
	//
	// If Type is GRPCLB, ServerName should be the name of the remote load
	// balancer, not the name of the backend.
	//
	// WARNING: ServerName must only be populated with trusted values. It
	// is insecure to populate it with data from untrusted inputs since untrusted
	// values could be used to bypass the authority checks performed by TLS.
	ServerName string

	// Attributes contains arbitrary data about this address intended for
	// consumption by the load balancing policy.
	Attributes *attributes.Attributes

	// Type is the type of this address.
	//
	// Deprecated: use Attributes instead.
	Type AddressType

	// Metadata is the information associated with Addr, which may be used
	// to make load balancing decision.
	//
	// Deprecated: use Attributes instead.
	Metadata interface{}
}

// BuildOptions 包括供构建器创建解析器的其他信息。
type BuildOptions struct {
	// DisableServiceConfig indicates whether a resolver implementation should
	// fetch service config data.
	DisableServiceConfig bool
	// DialCreds is the transport credentials used by the ClientConn for
	// communicating with the target gRPC service (set via
	// WithTransportCredentials). In cases where a name resolution service
	// requires the same credentials, the resolver may use this field. In most
	// cases though, it is not appropriate, and this field may be ignored.
	DialCreds credentials.TransportCredentials
	// CredsBundle is the credentials bundle used by the ClientConn for
	// communicating with the target gRPC service (set via
	// WithCredentialsBundle). In cases where a name resolution service
	// requires the same credentials, the resolver may use this field. In most
	// cases though, it is not appropriate, and this field may be ignored.
	CredsBundle credentials.Bundle
	// Dialer is the custom dialer used by the ClientConn for dialling the
	// target gRPC service (set via WithDialer). In cases where a name
	// resolution service requires the same dialer, the resolver may use this
	// field. In most cases though, it is not appropriate, and this field may
	// be ignored.
	Dialer func(context.Context, string) (net.Conn, error)
}

// State 包含与ClientConn相关的当前解析器状态。
// 将返回值包装在 State 对象中，再通过回调如 UpdateState 方法进行返回
// 如 resolver 中名字解析发生变化时，通过 UpdateState(State) 函数的回调告知 ClientConn 对象。
type State struct {
	// Addresses 是 target 最新一组已解析地址。
	Addresses []Address

	// ServiceConfig contains the result from parsing the latest service
	// config.  If it is nil, it indicates no service config is present or the
	// resolver does not provide service configs.
	ServiceConfig *serviceconfig.ParseResult

	// Attributes contains arbitrary data about the resolver intended for
	// consumption by the load balancing policy.
	Attributes *attributes.Attributes
}


//定义了自己的 ClientConn 接口
//包含解析程序的回调，以通知对gRPC ClientConn的任何更新，grpc内部使用的接口
//调用侧的 ClientConn 并非真正的 clientConn 对象，包装类 ccResolverWrapper，对 resolver UpdateState() 做胶水转换，再去调用 clientConn 对象的私有方法。
//相同的操作在 balancer 上也有体现。
type ClientConn interface {
	// 适当地更新ClientConn的状态。
	UpdateState(State)

	//ReportError通知ClientConn解析程序遇到了错误。
	//ClientConn将通知负载平衡器，并开始以指数退避的方式在Resolver上调用ResolveNow（触发名词解析）。
	ReportError(error)

	//解析程序调用NewAddress来通知ClientConn解析地址的新列表。地址列表应该是已解析地址的完整列表。
	NewAddress(addresses []Address)

	//由解析程序调用，以通知ClientConn新的服务配置。服务配置应作为json字符串提供
	NewServiceConfig(serviceConfig string)
	//ParseServiceConfig解析提供的服务配置，并返回提供解析后的配置的对象。
	ParseServiceConfig(serviceConfigJSON string) *serviceconfig.ParseResult
}

// Target represents a target for gRPC, as specified in:
// https://github.com/grpc/grpc/blob/master/doc/naming.md.
// It is parsed from the target string that gets passed into Dial or DialContext by the user. And
// grpc passes it to the resolver and the balancer.
//
// If the target follows the naming spec, and the parsed scheme is registered with grpc, we will
// parse the target string according to the spec. e.g. "dns://some_authority/foo.bar" will be parsed
// into &Target{Scheme: "dns", Authority: "some_authority", Endpoint: "foo.bar"}
//
// If the target does not contain a scheme, we will apply the default scheme, and set the Target to
// be the full target string. e.g. "foo.bar" will be parsed into
// &Target{Scheme: resolver.GetDefaultScheme(), Endpoint: "foo.bar"}.
//
// If the parsed scheme is not registered (i.e. no corresponding resolver available to resolve the
// endpoint), we set the Scheme to be the default scheme, and set the Endpoint to be the full target
// string. e.g. target string "unknown_scheme://authority/endpoint" will be parsed into
// &Target{Scheme: resolver.GetDefaultScheme(), Endpoint: "unknown_scheme://authority/endpoint"}.
type Target struct {
	Scheme    string
	Authority string
	Endpoint  string
}

// Builder 创建一个解析器，该解析器将用于监视名称解析更新。
//像个 Factory 类，用于创建具体的 Resolver 对象
type Builder interface {
	// Build creates a new resolver for the given target.
	//
	// gRPC dial calls Build synchronously, and fails if the returned error is
	// not nil.
	Build(target Target, cc ClientConn, opts BuildOptions) (Resolver, error)
	// Scheme returns the scheme supported by this resolver.
	// Scheme is defined at https://github.com/grpc/grpc/blob/master/doc/naming.md.
	Scheme() string
}

// ResolveNowOptions includes additional information for ResolveNow.
type ResolveNowOptions struct{}

// Resolver 监视指定目标上的更新。更新包括地址更新和服务配置更新。
type Resolver interface {
	//ClientConn 对象会通过 Resolver 接口的 ResolveNow 方法来触发名字解析
	//支持并发调用
	ResolveNow(ResolveNowOptions)
	// Close closes the resolver.
	Close()
}

// UnregisterForTesting removes the resolver builder with the given scheme from the
// resolver map.
// This function is for testing only.
func UnregisterForTesting(scheme string) {
	delete(m, scheme)
}