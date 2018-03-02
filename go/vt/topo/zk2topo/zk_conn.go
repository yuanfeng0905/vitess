/*
Copyright 2017 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package zk2topo

import (
	"flag"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	log "github.com/golang/glog"
	"github.com/samuel/go-zookeeper/zk"
	"golang.org/x/net/context"

	"vitess.io/vitess/go/netutil"
	"vitess.io/vitess/go/sync2"
)

const (
	// maxAttempts is how many times we retry queries.  At 2 for
	// now, so if a query fails because the session expired, we
	// just try to reconnect once and go on.
	maxAttempts = 2

	// PermDirectory are default permissions for a node.
	PermDirectory = zk.PermAdmin | zk.PermCreate | zk.PermDelete | zk.PermRead | zk.PermWrite

	// PermFile allows a zk node to emulate file behavior by
	// disallowing child nodes.
	PermFile = zk.PermAdmin | zk.PermRead | zk.PermWrite
)

var (
	maxConcurrency = flag.Int("topo_zk_max_concurrency", 64, "maximum number of pending requests to send to a Zookeeper server.")

	baseTimeout = flag.Duration("topo_zk_base_timeout", 30*time.Second, "zk base timeout (see zk.Connect)")
)

// Time returns a time.Time from a ZK int64 milliseconds since Epoch time.
func Time(i int64) time.Time {
	return time.Unix(i/1000, i%1000*1000000)
}

// ZkTime returns a ZK time (int64) from a time.Time
func ZkTime(t time.Time) int64 {
	return t.Unix()*1000 + int64(t.Nanosecond()/1000000)
}

// ZkConn is a wrapper class on top of a zk.Conn.
// It will do a few things for us:
// - add the context parameter. However, we do not enforce its deadlines
//   necessarily.
// - enforce a max concurrency of access to Zookeeper. We just don't
//   want to make too many calls concurrently, to not take too many resources.
// - retry some calls to Zookeeper. If we were disconnected from the
//   server, we want to try connecting again before failing.
type ZkConn struct {
	// addr is set at construction time, and immutable.
	addr string

	// sem protects concurrent calls to Zookeeper.
	sem *sync2.Semaphore

	// mu protects the following fields.
	mu   sync.Mutex
	conn *zk.Conn
}

func Connect(addr string) *ZkConn {
	return &ZkConn{
		addr: addr,
		sem:  sync2.NewSemaphore(*maxConcurrency, 0),
	}
}

// Get is part of the Conn interface.
func (c *ZkConn) Get(ctx context.Context, path string) (data []byte, stat *zk.Stat, err error) {
	err = c.withRetry(ctx, func(conn *zk.Conn) error {
		data, stat, err = conn.Get(path)
		return err
	})
	return
}

// GetW is part of the Conn interface.
func (c *ZkConn) GetW(ctx context.Context, path string) (data []byte, stat *zk.Stat, watch <-chan zk.Event, err error) {
	err = c.withRetry(ctx, func(conn *zk.Conn) error {
		data, stat, watch, err = conn.GetW(path)
		return err
	})
	return
}

// Children is part of the Conn interface.
func (c *ZkConn) Children(ctx context.Context, path string) (children []string, stat *zk.Stat, err error) {
	err = c.withRetry(ctx, func(conn *zk.Conn) error {
		children, stat, err = conn.Children(path)
		return err
	})
	return
}

// ChildrenW is part of the Conn interface.
func (c *ZkConn) ChildrenW(ctx context.Context, path string) (children []string, stat *zk.Stat, watch <-chan zk.Event, err error) {
	err = c.withRetry(ctx, func(conn *zk.Conn) error {
		children, stat, watch, err = conn.ChildrenW(path)
		return err
	})
	return
}

// Exists is part of the Conn interface.
func (c *ZkConn) Exists(ctx context.Context, path string) (exists bool, stat *zk.Stat, err error) {
	err = c.withRetry(ctx, func(conn *zk.Conn) error {
		exists, stat, err = conn.Exists(path)
		return err
	})
	return
}

// ExistsW is part of the Conn interface.
func (c *ZkConn) ExistsW(ctx context.Context, path string) (exists bool, stat *zk.Stat, watch <-chan zk.Event, err error) {
	err = c.withRetry(ctx, func(conn *zk.Conn) error {
		exists, stat, watch, err = conn.ExistsW(path)
		return err
	})
	return
}

// Create is part of the Conn interface.
func (c *ZkConn) Create(ctx context.Context, path string, value []byte, flags int32, aclv []zk.ACL) (pathCreated string, err error) {
	err = c.withRetry(ctx, func(conn *zk.Conn) error {
		pathCreated, err = conn.Create(path, value, flags, aclv)
		return err
	})
	return
}

// Set is part of the Conn interface.
func (c *ZkConn) Set(ctx context.Context, path string, value []byte, version int32) (stat *zk.Stat, err error) {
	err = c.withRetry(ctx, func(conn *zk.Conn) error {
		stat, err = conn.Set(path, value, version)
		return err
	})
	return
}

// Delete is part of the Conn interface.
func (c *ZkConn) Delete(ctx context.Context, path string, version int32) error {
	return c.withRetry(ctx, func(conn *zk.Conn) error {
		return conn.Delete(path, version)
	})
}

// GetACL is part of the Conn interface.
func (c *ZkConn) GetACL(ctx context.Context, path string) (aclv []zk.ACL, stat *zk.Stat, err error) {
	err = c.withRetry(ctx, func(conn *zk.Conn) error {
		aclv, stat, err = conn.GetACL(path)
		return err
	})
	return
}

// SetACL is part of the Conn interface.
func (c *ZkConn) SetACL(ctx context.Context, path string, aclv []zk.ACL, version int32) error {
	return c.withRetry(ctx, func(conn *zk.Conn) error {
		_, err := conn.SetACL(path, aclv, version)
		return err
	})
}

// Close is part of the Conn interface.
func (c *ZkConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
	}
	return nil
}

// withRetry encapsulates the retry logic and concurrent access to
// Zookeeper.
//
// Some errors are not handled gracefully by the Zookeeper client. This is
// sort of odd, but in general it doesn't affect the kind of code you
// need to have a truly reliable client.
//
// However, it can manifest itself as an annoying transient error that
// is likely avoidable when trying simple operations like Get.
// To that end, we retry when possible to minimize annoyance at
// higher levels.
//
// https://issues.apache.org/jira/browse/ZOOKEEPER-22
func (c *ZkConn) withRetry(ctx context.Context, action func(conn *zk.Conn) error) (err error) {

	// Handle concurrent access to a Zookeeper server here.
	c.sem.Acquire()
	defer c.sem.Release()

	for i := 0; i < maxAttempts; i++ {
		if i > 0 {
			// Add a bit of backoff time before retrying:
			// 1 second base + up to 5 seconds.
			time.Sleep(1*time.Second + time.Duration(rand.Int63n(5e9)))
		}

		// Get the current connection, or connect.
		var conn *zk.Conn
		conn, err = c.getConn(ctx)
		if err != nil {
			// We can't connect, try again.
			continue
		}

		// Execute the action.
		err = action(conn)
		if err != zk.ErrConnectionClosed {
			// It worked, or it failed for another reason
			// than connection related.
			return
		}

		// We got an error, because the connection was closed.
		// Let's clear up our errored connection and try again.
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
	}
	return
}

// getConn returns the connection in a thread safe way. It will try to connect
// if not connected yet.
func (c *ZkConn) getConn(ctx context.Context) (*zk.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		conn, events, err := dialZk(ctx, c.addr)
		if err != nil {
			return nil, err
		}
		c.conn = conn
		go c.handleSessionEvents(conn, events)
	}
	return c.conn, nil
}

// handleSessionEvents is processing events from the session channel.
// When it detects that the connection is not working any more, it
// clears out the connection record.
func (c *ZkConn) handleSessionEvents(conn *zk.Conn, session <-chan zk.Event) {
	for event := range session {
		closeRequired := false

		switch event.State {
		case zk.StateExpired, zk.StateConnecting:
			closeRequired = true
			fallthrough
		case zk.StateDisconnected:
			c.mu.Lock()
			if c.conn == conn {
				// The ZkConn still references this
				// connection, let's nil it.
				c.conn = nil
			}
			c.mu.Unlock()
			if closeRequired {
				conn.Close()
			}
			log.Infof("zk conn: session for addr %v ended: %v", c.addr, event)
			return
		}
		log.Infof("zk conn: session for addr %v event: %v", c.addr, event)
	}
}

// dialZk dials the server, and waits until connection.
func dialZk(ctx context.Context, addr string) (*zk.Conn, <-chan zk.Event, error) {
	servers, err := resolveZkAddr(addr)
	if err != nil {
		return nil, nil, err
	}

	zconn, session, err := zk.Connect(servers, *baseTimeout)
	if err != nil {
		return nil, nil, err
	}

	// Wait for connection, skipping transition states.
	for {
		select {
		case <-ctx.Done():
			zconn.Close()
			return nil, nil, ctx.Err()
		case event := <-session:
			switch event.State {
			case zk.StateConnected:
				// success
				return zconn, session, nil

			case zk.StateAuthFailed:
				// fast fail this one
				zconn.Close()
				return nil, nil, fmt.Errorf("zk connect failed: StateAuthFailed")
			}
		}
	}
}

// resolveZkAddr takes a comma-separated list of host:port addresses,
// and resolves the host to replace it with the IP address.
// If a resolution fails, the host is skipped.
// If no host can be resolved, an error is returned.
// This is different from the Zookeeper library, that insists on resolving
// *all* hosts successfully before it starts.
func resolveZkAddr(zkAddr string) ([]string, error) {
	parts := strings.Split(zkAddr, ",")
	resolved := make([]string, 0, len(parts))
	for _, part := range parts {
		// The Zookeeper client cannot handle IPv6 addresses before version 3.4.x.
		if r, err := netutil.ResolveIPv4Addr(part); err != nil {
			log.Warningf("cannot resolve %v, will not use it: %v", part, err)
		} else {
			resolved = append(resolved, r)
		}
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("no valid address found in %v", zkAddr)
	}
	return resolved, nil
}
