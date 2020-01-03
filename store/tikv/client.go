// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

// Package tikv provides tcp connection to kvserver.
package tikv

import (
	"context"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/coprocessor"
	"github.com/pingcap/kvproto/pkg/tikvpb"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/store/tikv/tikvrpc"
	"github.com/pingcap/tidb/util/logutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/keepalive"
)

// MaxRecvMsgSize set max gRPC receive message size received from server. If any message size is larger than
// current value, an error will be reported from gRPC.
var MaxRecvMsgSize = math.MaxInt64

// Timeout durations.
var (
	dialTimeout               = 5 * time.Second
	readTimeoutShort          = 20 * time.Second  // For requests that read/write several key-values.
	ReadTimeoutMedium         = 60 * time.Second  // For requests that may need scan region.
	ReadTimeoutLong           = 150 * time.Second // For requests that may need scan region multiple times.
	GCTimeout                 = 5 * time.Minute
	UnsafeDestroyRangeTimeout = 5 * time.Minute
)

const (
	grpcInitialWindowSize     = 1 << 30
	grpcInitialConnWindowSize = 1 << 30
)

// Client is a client that sends RPC.
// It should not be used after calling Close().
type Client interface {
	// Close should release all data.
	Close() error
	// SendRequest sends Request.
	SendRequest(ctx context.Context, addr string, req *tikvrpc.Request, timeout time.Duration) (*tikvrpc.Response, error)
}

type connArray struct {
	// The target host.
	target string

	index uint32
	v     []*grpc.ClientConn
	// streamTimeout binds with a background goroutine to process coprocessor streaming timeout.
	streamTimeout chan *tikvrpc.Lease
	done          chan struct{}
}

func newConnArray(maxSize uint, addr string, idleNotify *uint32) (*connArray, error) {
	a := &connArray{
		index:         0,
		v:             make([]*grpc.ClientConn, maxSize),
		streamTimeout: make(chan *tikvrpc.Lease, 1024),
		done:          make(chan struct{}),
	}
	if err := a.Init(addr, idleNotify); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *connArray) Init(addr string, idleNotify *uint32) error {
	a.target = addr

	opt := grpc.WithInsecure()

	cfg := config.GetGlobalConfig()
	var (
		unaryInterceptor  grpc.UnaryClientInterceptor
		streamInterceptor grpc.StreamClientInterceptor
	)

	keepAlive := cfg.TiKVClient.GrpcKeepAliveTime
	keepAliveTimeout := cfg.TiKVClient.GrpcKeepAliveTimeout
	for i := range a.v {
		ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
		conn, err := grpc.DialContext(
			ctx,
			addr,
			opt,
			grpc.WithInitialWindowSize(grpcInitialWindowSize),
			grpc.WithInitialConnWindowSize(grpcInitialConnWindowSize),
			grpc.WithUnaryInterceptor(unaryInterceptor),
			grpc.WithStreamInterceptor(streamInterceptor),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxRecvMsgSize)),
			grpc.WithConnectParams(grpc.ConnectParams{
				Backoff: backoff.Config{
					BaseDelay:  100 * time.Millisecond, // Default was 1s.
					Multiplier: 1.6,                    // Default
					Jitter:     0.2,                    // Default
					MaxDelay:   3 * time.Second,        // Default was 120s.
				},
				MinConnectTimeout: dialTimeout,
			}),
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time:                time.Duration(keepAlive) * time.Second,
				Timeout:             time.Duration(keepAliveTimeout) * time.Second,
				PermitWithoutStream: true,
			}),
		)
		cancel()
		if err != nil {
			// Cleanup if the initialization fails.
			a.Close()
			return errors.Trace(err)
		}
		a.v[i] = conn
	}
	go tikvrpc.CheckStreamTimeoutLoop(a.streamTimeout, a.done)

	return nil
}

func (a *connArray) Get() *grpc.ClientConn {
	next := atomic.AddUint32(&a.index, 1) % uint32(len(a.v))
	return a.v[next]
}

func (a *connArray) Close() {
	for i, c := range a.v {
		if c != nil {
			err := c.Close()
			terror.Log(errors.Trace(err))
			a.v[i] = nil
		}
	}

	close(a.done)
}

// rpcClient is RPC client struct.
// TODO: Add flow control between RPC clients in TiDB ond RPC servers in TiKV.
// Since we use shared client connection to communicate to the same TiKV, it's possible
// that there are too many concurrent requests which overload the service of TiKV.
type rpcClient struct {
	sync.RWMutex

	conns map[string]*connArray

	idleNotify uint32
	// Periodically check whether there is any connection that is idle and then close and remove these idle connections.
	// Implement background cleanup.
	isClosed bool
}

func newRPCClient() *rpcClient {
	return &rpcClient{
		conns: make(map[string]*connArray),
	}
}

// NewTestRPCClient is for some external tests.
func NewTestRPCClient() Client {
	return newRPCClient()
}

func (c *rpcClient) getConnArray(addr string) (*connArray, error) {
	c.RLock()
	if c.isClosed {
		c.RUnlock()
		return nil, errors.Errorf("rpcClient is closed")
	}
	array, ok := c.conns[addr]
	c.RUnlock()
	if !ok {
		var err error
		array, err = c.createConnArray(addr)
		if err != nil {
			return nil, err
		}
	}
	return array, nil
}

func (c *rpcClient) createConnArray(addr string) (*connArray, error) {
	c.Lock()
	defer c.Unlock()
	array, ok := c.conns[addr]
	if !ok {
		var err error
		connCount := config.GetGlobalConfig().TiKVClient.GrpcConnectionCount
		array, err = newConnArray(connCount, addr, &c.idleNotify)
		if err != nil {
			return nil, err
		}
		c.conns[addr] = array
	}
	return array, nil
}

func (c *rpcClient) closeConns() {
	c.Lock()
	if !c.isClosed {
		c.isClosed = true
		// close all connections
		for _, array := range c.conns {
			array.Close()
		}
	}
	c.Unlock()
}

// SendRequest sends a Request to server and receives Response.
func (c *rpcClient) SendRequest(ctx context.Context, addr string, req *tikvrpc.Request, timeout time.Duration) (*tikvrpc.Response, error) {
	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("rpcClient.SendRequest", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		ctx = opentracing.ContextWithSpan(ctx, span1)
	}

	connArray, err := c.getConnArray(addr)
	if err != nil {
		return nil, errors.Trace(err)
	}

	clientConn := connArray.Get()
	client := tikvpb.NewTikvClient(clientConn)

	if req.Type != tikvrpc.CmdCopStream {
		ctx1, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return tikvrpc.CallRPC(ctx1, client, req)
	}

	// Coprocessor streaming request.
	// Use context to support timeout for grpc streaming client.
	ctx1, cancel := context.WithCancel(ctx)
	// Should NOT call defer cancel() here because it will cancel further stream.Recv()
	// We put it in copStream.Lease.Cancel call this cancel at copStream.Close
	// TODO: add unit test for SendRequest.
	resp, err := tikvrpc.CallRPC(ctx1, client, req)
	if err != nil {
		cancel()
		return nil, errors.Trace(err)
	}

	// Put the lease object to the timeout channel, so it would be checked periodically.
	copStream := resp.Resp.(*tikvrpc.CopStreamResponse)
	copStream.Timeout = timeout
	copStream.Lease.Cancel = cancel
	connArray.streamTimeout <- &copStream.Lease

	// Read the first streaming response to get CopStreamResponse.
	// This can make error handling much easier, because SendReq() retry on
	// region error automatically.
	var first *coprocessor.Response
	first, err = copStream.Recv()
	if err != nil {
		if errors.Cause(err) != io.EOF {
			return nil, errors.Trace(err)
		}
		logutil.BgLogger().Debug("copstream returns nothing for the request.")
	}
	copStream.Response = first
	return resp, nil
}

func (c *rpcClient) Close() error {
	// TODO: add a unit test for SendRequest After Closed
	c.closeConns()
	return nil
}
