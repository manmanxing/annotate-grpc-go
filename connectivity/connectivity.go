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

// Package connectivity 定义连接语义.
// For details, see https://github.com/grpc/grpc/blob/master/doc/connectivity-semantics-and-api.md.

package connectivity

import (
	"google.golang.org/grpc/grpclog"
)

var logger = grpclog.Component("core")

//指示连接状态。它可以是ClientConn或SubConn的状态
type State int

func (s State) String() string {
	switch s {
	case Idle:
		return "IDLE"
	case Connecting:
		return "CONNECTING"
	case Ready:
		return "READY"
	case TransientFailure:
		return "TRANSIENT_FAILURE"
	case Shutdown:
		return "SHUTDOWN"
	default:
		logger.Errorf("unknown connectivity state: %d", s)
		return "Invalid-State"
	}
}

const (
	//表示ClientConn处于空闲状态
	Idle State = iota
	//表示ClientConn正在连接。
	Connecting
	//表示ClientConn已准备就绪
	Ready
	//表示ClientConn发生故障，但希望恢复
	TransientFailure
	//表示ClientConn已开始关闭。
	Shutdown
)