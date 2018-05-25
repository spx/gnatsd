// Copyright 2012-2018 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Type of client connection.
const (
	// CLIENT is an end user.
	CLIENT = iota
	// ROUTER is another router in the cluster.
	ROUTER
)

const (
	// Original Client protocol from 2009.
	// http://nats.io/documentation/internals/nats-protocol/
	ClientProtoZero = iota
	// This signals a client can receive more then the original INFO block.
	// This can be used to update clients on other cluster members, etc.
	ClientProtoInfo
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

const (
	// Scratch buffer size for the processMsg() calls.
	msgScratchSize = 512
	msgHeadProto   = "MSG "
)

// For controlling dynamic buffer sizes.
const (
	startBufSize = 512   // For INFO/CONNECT block
	minBufSize   = 64    // Smallest to shrink to for PING/PONG
	maxBufSize   = 65536 // 64k
)

// Represent client booleans with a bitmask
type clientFlag byte

// Some client state represented as flags
const (
	connectReceived   clientFlag = 1 << iota // The CONNECT proto has been received
	firstPongSent                            // The first PONG has been sent
	handshakeComplete                        // For TLS clients, indicate that the handshake is complete
	clearConnection                          // Marks that clearConnection has already been called.
	flushOutbound                            // Marks client as having a flushOutbound call in progress.
)

// set the flag (would be equivalent to set the boolean to true)
func (cf *clientFlag) set(c clientFlag) {
	*cf |= c
}

// clear the flag (would be equivalent to set the boolean to false)
func (cf *clientFlag) clear(c clientFlag) {
	*cf &= ^c
}

// isSet returns true if the flag is set, false otherwise
func (cf clientFlag) isSet(c clientFlag) bool {
	return cf&c != 0
}

// setIfNotSet will set the flag `c` only if that flag was not already
// set and return true to indicate that the flag has been set. Returns
// false otherwise.
func (cf *clientFlag) setIfNotSet(c clientFlag) bool {
	if *cf&c == 0 {
		*cf |= c
		return true
	}
	return false
}

type client struct {
	// Here first because of use of atomics, and memory alignment.
	stats
	mpay  int64
	mu    sync.Mutex
	typ   int
	cid   uint64
	opts  clientOpts
	start time.Time
	nc    net.Conn
	ncs   string
	out   outbound
	srv   *Server
	subs  map[string]*subscription
	perms *permissions
	cache readCache
	pcd   map[*client]struct{}
	atmr  *time.Timer
	ptmr  *time.Timer
	pout  int
	msgb  [msgScratchSize]byte
	last  time.Time
	parseState

	route *route
	debug bool
	trace bool

	flags clientFlag // Compact booleans into a single field. Size will be increased when needed.
}

// outbound holds pending data for a socket.
type outbound struct {
	p   []byte        // Primary write buffer
	s   []byte        // Secondary for use post flush
	nb  net.Buffers   // net.Buffers for writev IO
	sz  int           // limit size per []byte, uses variable BufSize constants, start, min, max.
	pb  int64         // Total pending/queued bytes.
	sg  *sync.Cond    // Flusher conditional for signaling.
	fps int           // Flush pending signals from inbound messages.
	lft time.Duration // Last flush time.
}

type permissions struct {
	sub    *Sublist
	pub    *Sublist
	pcache map[string]bool
}

const (
	maxResultCacheSize = 512
	maxPermCacheSize   = 32
	pruneSize          = 16
)

// Used in readloop to cache hot subject lookups and group statistics.
type readCache struct {
	genid   uint64
	results map[string]*SublistResult
	prand   *rand.Rand
	inMsgs  int
	inBytes int
	subs    int
}

func (c *client) String() (id string) {
	return c.ncs
}

func (c *client) GetOpts() *clientOpts {
	return &c.opts
}

// GetTLSConnectionState returns the TLS ConnectionState if TLS is enabled, nil
// otherwise. Implements the ClientAuth interface.
func (c *client) GetTLSConnectionState() *tls.ConnectionState {
	tc, ok := c.nc.(*tls.Conn)
	if !ok {
		return nil
	}
	state := tc.ConnectionState()
	return &state
}

type subscription struct {
	client  *client
	subject []byte
	queue   []byte
	sid     []byte
	nm      int64
	max     int64
}

type clientOpts struct {
	Verbose       bool   `json:"verbose"`
	Pedantic      bool   `json:"pedantic"`
	TLSRequired   bool   `json:"tls_required"`
	Authorization string `json:"auth_token"`
	Username      string `json:"user"`
	Password      string `json:"pass"`
	Name          string `json:"name"`
	Lang          string `json:"lang"`
	Version       string `json:"version"`
	Protocol      int    `json:"protocol"`
}

var defaultOpts = clientOpts{Verbose: true, Pedantic: true}

func init() {
	rand.Seed(time.Now().UnixNano())
}

// Lock should be held
func (c *client) initClient() {
	s := c.srv
	c.cid = atomic.AddUint64(&s.gcid, 1)

	// Outbound data structure setup
	c.out.sz = startBufSize
	c.out.sg = sync.NewCond(&c.mu)

	c.subs = make(map[string]*subscription)
	c.debug = (atomic.LoadInt32(&c.srv.logging.debug) != 0)
	c.trace = (atomic.LoadInt32(&c.srv.logging.trace) != 0)

	// This is a scratch buffer used for processMsg()
	// The msg header starts with "MSG ",
	// in bytes that is [77 83 71 32].
	c.msgb = [msgScratchSize]byte{77, 83, 71, 32}

	// This is to track pending clients that have data to be flushed
	// after we process inbound msgs from our own connection.
	c.pcd = make(map[*client]struct{})

	// snapshot the string version of the connection
	conn := "-"
	if ip, ok := c.nc.(*net.TCPConn); ok {
		addr := ip.RemoteAddr().(*net.TCPAddr)
		conn = fmt.Sprintf("%s:%d", addr.IP, addr.Port)
	}

	switch c.typ {
	case CLIENT:
		c.ncs = fmt.Sprintf("%s - cid:%d", conn, c.cid)
	case ROUTER:
		c.ncs = fmt.Sprintf("%s - rid:%d", conn, c.cid)
	}
}

// RegisterUser allows auth to call back into a new client
// with the authenticated user. This is used to map any permissions
// into the client.
func (c *client) RegisterUser(user *User) {
	if user.Permissions == nil {
		// Reset perms to nil in case client previously had them.
		c.mu.Lock()
		c.perms = nil
		c.mu.Unlock()
		return
	}

	// Process Permissions and map into client connection structures.
	c.mu.Lock()
	defer c.mu.Unlock()

	// Pre-allocate all to simplify checks later.
	c.perms = &permissions{}
	c.perms.sub = NewSublist()
	c.perms.pub = NewSublist()
	c.perms.pcache = make(map[string]bool)

	// Loop over publish permissions
	for _, pubSubject := range user.Permissions.Publish {
		sub := &subscription{subject: []byte(pubSubject)}
		c.perms.pub.Insert(sub)
	}

	// Loop over subscribe permissions
	for _, subSubject := range user.Permissions.Subscribe {
		sub := &subscription{subject: []byte(subSubject)}
		c.perms.sub.Insert(sub)
	}
}

// writeLoop is the main socket write functionality.
// Runs in its own Go routine.
func (c *client) writeLoop() {
	defer c.srv.grWG.Done()

	// Main loop. Will wait to be signaled and then will use
	// buffered outbound structure for efficient writev to the underlying socket.
	for {
		c.mu.Lock()
		if (c.out.pb == 0 || c.out.fps > 0) && !c.flags.isSet(clearConnection) {
			// Wait on pending data.
			c.out.sg.Wait()
		}
		// Flush data
		c.flushOutbound()
		isClosed := c.flags.isSet(clearConnection)
		c.mu.Unlock()

		// Check for closed condition.
		if isClosed {
			return
		}
	}
}

// readLoop is the main socket read functionality.
// Runs in its own Go routine.
func (c *client) readLoop() {
	// Grab the connection off the client, it will be cleared on a close.
	// We check for that after the loop, but want to avoid a nil dereference
	c.mu.Lock()
	nc := c.nc
	s := c.srv
	defer s.grWG.Done()
	c.mu.Unlock()

	if nc == nil {
		return
	}

	// Start read buffer.
	b := make([]byte, startBufSize)

	for {
		n, err := nc.Read(b)
		if err != nil {
			c.closeConnection()
			return
		}

		// Grab for updates for last activity.
		last := time.Now()

		// Clear inbound stats cache
		c.cache.inMsgs = 0
		c.cache.inBytes = 0
		c.cache.subs = 0

		// Main call into parser for inbound data. This will generate callouts
		// to process messages, etc.
		if err := c.parse(b[:n]); err != nil {
			// handled inline
			if err != ErrMaxPayload && err != ErrAuthorization {
				c.Errorf("Error reading from client: %s", err.Error())
				c.sendErr("Parser Error")
				c.closeConnection()
			}
			return
		}

		// Updates stats for client and server that were collected
		// from parsing through the buffer.
		atomic.AddInt64(&c.inMsgs, int64(c.cache.inMsgs))
		atomic.AddInt64(&c.inBytes, int64(c.cache.inBytes))
		atomic.AddInt64(&s.inMsgs, int64(c.cache.inMsgs))
		atomic.AddInt64(&s.inBytes, int64(c.cache.inBytes))

		budget := 5 * time.Millisecond

		// Check pending clients for flush.
		for cp := range c.pcd {
			// Queue up a flush for those in the set
			cp.mu.Lock()
			cp.out.fps--
			if budget > 0 {
				cp.flushOutbound()
				budget -= cp.out.lft
			} else {
				cp.flushSignal()
			}

			cp.mu.Unlock()
			delete(c.pcd, cp)
		}
		// Check to see if we got closed, e.g. slow consumer
		c.mu.Lock()
		nc := c.nc
		// Activity based on interest changes or data/msgs.
		if c.cache.inMsgs > 0 || c.cache.subs > 0 {
			c.last = last
		}

		c.mu.Unlock()
		if nc == nil {
			return
		}

		// Update read buffer size as/if needed.

		// Grow
		if n == len(b) && len(b) < maxBufSize {
			b = make([]byte, len(b)*2)
		}

		// Shrink, for now don't accelerate, ping/pong will eventually sort it out.
		if n < len(b)/2 && len(b) > minBufSize {
			b = make([]byte, len(b)/2)
		}
	}
}

// collapsePtoNB will place primary onto nb buffer as needed in prep for WriteTo.
// This will return a copy on purpose.
func (c *client) collapsePtoNB() net.Buffers {
	if c.out.p != nil {
		p := c.out.p
		c.out.p = nil
		return append(c.out.nb, p)
	}
	return c.out.nb
}

// This will handle the fixup needed on a partial write.
// Assume pending has been already calculated correctly.
func (c *client) handlePartialWrite(pnb net.Buffers) {
	nb := c.collapsePtoNB()
	// The partial needs to be first, so append nb to pnb
	c.out.nb = append(pnb, nb...)
}

// flushOutbound will flush outbound buffer to a client.
// Lock must be held
func (c *client) flushOutbound() {
	if c.flags.isSet(flushOutbound) {
		return
	}
	c.flags.set(flushOutbound)
	defer c.flags.clear(flushOutbound)

	// Check for nothing to do.
	if c.nc == nil || c.srv == nil || c.out.pb == 0 {
		return
	}

	// Snapshot opts
	srv := c.srv
	opts := srv.getOpts()

	// Place primary on nb, assign primary to secondary, nil out nb and secondary.
	nb := c.collapsePtoNB()
	c.out.p, c.out.nb, c.out.s = c.out.s, nil, nil

	// For selecting primary replacement.
	cnb := nb

	// In case it goes away after releasing the lock.
	nc := c.nc
	attempted := c.out.pb

	// Do NOT hold lock during actual IO
	c.mu.Unlock()

	// flush here
	now := time.Now()
	// FIXME(dlc) - writev will do multiple IOs past 1024 on
	// most platforms, need to account for that with deadline?
	nc.SetWriteDeadline(now.Add(opts.WriteDeadline))
	// Actual write to the socket.
	n, err := nb.WriteTo(nc)
	nc.SetWriteDeadline(time.Time{})
	lft := time.Since(now)
	// Re-acquire client lock
	c.mu.Lock()

	// Update flush time statistics
	c.out.lft = lft
	// Subtract from pending bytes.
	c.out.pb -= n

	if n != attempted && n > 0 {
		c.handlePartialWrite(nb)
	}

	if err != nil {
		if n == 0 {
			c.out.pb -= attempted
		}
		c.clearConnection()
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			atomic.AddInt64(&srv.slowConsumers, 1)
			c.Noticef("Slow Consumer Detected")
		} else {
			c.Debugf("Error flushing: %v", err)
		}
		return
	}

	// Adjust based on what we wrote plus any pending.
	pt := int(n + c.out.pb)

	// Adjust sz as needed downward, keeping power of 2.
	// We do this at a slower rate, hence the pt*4.
	for pt*4 < c.out.sz && c.out.sz > minBufSize {
		c.out.sz >>= 1
	}
	// Adjust sz as needed upward, keeping power of 2.
	for pt > c.out.sz && c.out.sz < maxBufSize {
		c.out.sz <<= 1
	}

	// Check to see if we can reuse buffers.
	if len(cnb) > 0 {
		oldp := cnb[0][:0]
		if cap(oldp) == c.out.sz || cap(oldp) == c.out.sz*2 {
			// Replace primary or secondary if they are nil, reusing same buffer.
			if c.out.p == nil {
				c.out.p = oldp
			} else if c.out.s == nil {
				c.out.s = oldp
			}
		}
	}
}

// flushSignal will use server to queue the flush IO operation to a pool of flushers.
// Lock must be held.
func (c *client) flushSignal() {
	c.out.sg.Signal()
}

func (c *client) traceMsg(msg []byte) {
	if !c.trace {
		return
	}
	// FIXME(dlc), allow limits to printable payload
	c.Tracef("->> MSG_PAYLOAD: [%s]", string(msg[:len(msg)-LEN_CR_LF]))
}

func (c *client) traceInOp(op string, arg []byte) {
	c.traceOp("->> %s", op, arg)
}

func (c *client) traceOutOp(op string, arg []byte) {
	c.traceOp("<<- %s", op, arg)
}

func (c *client) traceOp(format, op string, arg []byte) {
	if !c.trace {
		return
	}

	opa := []interface{}{}
	if op != "" {
		opa = append(opa, op)
	}
	if arg != nil {
		opa = append(opa, string(arg))
	}
	c.Tracef(format, opa)
}

// Process the information messages from Clients and other Routes.
func (c *client) processInfo(arg []byte) error {
	info := Info{}
	if err := json.Unmarshal(arg, &info); err != nil {
		return err
	}
	if c.typ == ROUTER {
		c.processRouteInfo(&info)
	}
	return nil
}

func (c *client) processErr(errStr string) {
	switch c.typ {
	case CLIENT:
		c.Errorf("Client Error %s", errStr)
	case ROUTER:
		c.Errorf("Route Error %s", errStr)
	}
	c.closeConnection()
}

func (c *client) processConnect(arg []byte) error {
	c.traceInOp("CONNECT", arg)

	c.mu.Lock()
	// If we can't stop the timer because the callback is in progress...
	if !c.clearAuthTimer() {
		// wait for it to finish and handle sending the failure back to
		// the client.
		for c.nc != nil {
			c.mu.Unlock()
			time.Sleep(25 * time.Millisecond)
			c.mu.Lock()
		}
		c.mu.Unlock()
		return nil
	}
	c.last = time.Now()
	typ := c.typ
	r := c.route
	srv := c.srv
	// Moved unmarshalling of clients' Options under the lock.
	// The client has already been added to the server map, so it is possible
	// that other routines lookup the client, and access its options under
	// the client's lock, so unmarshalling the options outside of the lock
	// would cause data RACEs.
	if err := json.Unmarshal(arg, &c.opts); err != nil {
		c.mu.Unlock()
		return err
	}
	// Indicate that the CONNECT protocol has been received, and that the
	// server now knows which protocol this client supports.
	c.flags.set(connectReceived)
	// Capture these under lock
	proto := c.opts.Protocol
	verbose := c.opts.Verbose
	lang := c.opts.Lang
	c.mu.Unlock()

	if srv != nil {
		// As soon as c.opts is unmarshalled and if the proto is at
		// least ClientProtoInfo, we need to increment the following counter.
		// This is decremented when client is removed from the server's
		// clients map.
		if proto >= ClientProtoInfo {
			srv.mu.Lock()
			srv.cproto++
			srv.mu.Unlock()
		}

		// Check for Auth
		if ok := srv.checkAuthorization(c); !ok {
			c.authViolation()
			return ErrAuthorization
		}
	}

	// Check client protocol request if it exists.
	if typ == CLIENT && (proto < ClientProtoZero || proto > ClientProtoInfo) {
		c.sendErr(ErrBadClientProtocol.Error())
		c.closeConnection()
		return ErrBadClientProtocol
	} else if typ == ROUTER && lang != "" {
		// Way to detect clients that incorrectly connect to the route listen
		// port. Client provide Lang in the CONNECT protocol while ROUTEs don't.
		c.sendErr(ErrClientConnectedToRoutePort.Error())
		c.closeConnection()
		return ErrClientConnectedToRoutePort
	}

	// Grab connection name of remote route.
	if typ == ROUTER && r != nil {
		c.mu.Lock()
		c.route.remoteID = c.opts.Name
		c.mu.Unlock()
	}

	if verbose {
		c.sendOK()
	}
	return nil
}

func (c *client) authTimeout() {
	c.sendErr(ErrAuthTimeout.Error())
	c.Debugf("Authorization Timeout")
	c.closeConnection()
}

func (c *client) authViolation() {
	if c.srv != nil && c.srv.getOpts().Users != nil {
		c.Errorf("%s - User %q",
			ErrAuthorization.Error(),
			c.opts.Username)
	} else {
		c.Errorf(ErrAuthorization.Error())
	}
	c.sendErr("Authorization Violation")
	c.closeConnection()
}

func (c *client) maxConnExceeded() {
	c.Errorf(ErrTooManyConnections.Error())
	c.sendErr(ErrTooManyConnections.Error())
	c.closeConnection()
}

func (c *client) maxPayloadViolation(sz int, max int64) {
	c.Errorf("%s: %d vs %d", ErrMaxPayload.Error(), sz, max)
	c.sendErr("Maximum Payload Violation")
	c.closeConnection()
}

// queueOutbound queues data for client/route connections.
// Return pending length.
// Lock should be held
func (c *client) queueOutbound(data []byte) {
	// Add to pending bytes total.
	c.out.pb += int64(len(data))

	// Check for slow consumer via pending bytes limit.
	// ok to return here, client is going away.
	if c.out.pb > c.srv.getOpts().MaxPending {
		c.clearConnection()
		atomic.AddInt64(&c.srv.slowConsumers, 1)
		c.Noticef("Slow Consumer Detected")
		return
	}

	if c.out.p == nil {
		if c.out.sz == 0 {
			c.out.sz = startBufSize
		}
		if c.out.s != nil && cap(c.out.s) >= c.out.sz {
			c.out.p = c.out.s
			c.out.s = nil
		} else {
			c.out.p = make([]byte, 0, c.out.sz)
		}
	}
	// Determine if we copy or reference
	if len(data) > cap(c.out.p)-len(c.out.p) {
		// Put the primary on the nb if it has a payload
		if len(c.out.p) > 0 {
			c.out.nb = append(c.out.nb, c.out.p)
			c.out.p = nil
		}
		// Check for a big message, and if found place directly on nb
		// FIXME(dlc) - do we need signaling of ownership here if we want len(data) < maxBufSize?

		if len(data) > maxBufSize {
			c.out.nb = append(c.out.nb, data)
		} else {
			// We will copy to primary.
			if c.out.p == nil {
				if len(data) > c.out.sz {
					c.out.p = make([]byte, 0, len(data))
				} else {
					if c.out.s != nil && cap(c.out.s) >= c.out.sz { // TODO(dlc) - Size mismatch?
						c.out.p = c.out.s
						c.out.s = nil
					} else {
						c.out.p = make([]byte, 0, c.out.sz)
					}
				}
			}
			c.out.p = append(c.out.p, data...)
		}
	} else {
		c.out.p = append(c.out.p, data...)
	}
}

// Assume the lock is held upon entry.
func (c *client) sendProto(info []byte, doFlush bool) {
	if c.nc == nil {
		return
	}
	c.queueOutbound(info)
	if doFlush {
		c.flushOutbound()
	} else {
		c.flushSignal()
	}
}

// Assume the lock is held upon entry.
func (c *client) sendInfo(info []byte) {
	c.sendProto(info, true)
}

func (c *client) sendErr(err string) {
	c.mu.Lock()
	c.traceOutOp("-ERR", []byte(err))
	c.sendProto([]byte(fmt.Sprintf("-ERR '%s'\r\n", err)), true)
	c.mu.Unlock()
}

func (c *client) sendOK() {
	c.mu.Lock()
	c.traceOutOp("OK", nil)
	// Can not autoflush this one, needs to be async.
	c.sendProto([]byte("+OK\r\n"), false)
	// FIXME(dlc) - ??
	c.pcd[c] = needFlush
	c.mu.Unlock()
}

func (c *client) processPing() {
	c.mu.Lock()
	c.traceInOp("PING", nil)
	if c.nc == nil {
		c.mu.Unlock()
		return
	}
	c.traceOutOp("PONG", nil)
	c.sendProto([]byte("PONG\r\n"), true)

	// The CONNECT should have been received, but make sure it
	// is so before proceeding
	if !c.flags.isSet(connectReceived) {
		c.mu.Unlock()
		return
	}
	// If we are here, the CONNECT has been received so we know
	// if this client supports async INFO or not.
	var (
		checkClusterChange bool
		srv                = c.srv
	)
	// For older clients, just flip the firstPongSent flag if not already
	// set and we are done.
	if c.opts.Protocol < ClientProtoInfo || srv == nil {
		c.flags.setIfNotSet(firstPongSent)
	} else {
		// This is a client that supports async INFO protocols.
		// If this is the first PING (so firstPongSent is not set yet),
		// we will need to check if there was a change in cluster topology.
		checkClusterChange = !c.flags.isSet(firstPongSent)
	}
	c.mu.Unlock()

	if checkClusterChange {
		srv.mu.Lock()
		c.mu.Lock()
		// Now that we are under both locks, we can flip the flag.
		// This prevents sendAsyncInfoToClients() and and code here
		// to send a double INFO protocol.
		c.flags.set(firstPongSent)
		// If there was a cluster update since this client was created,
		// send an updated INFO protocol now.
		if srv.lastCURLsUpdate >= c.start.UnixNano() {
			c.sendInfo(srv.infoJSON)
		}
		c.mu.Unlock()
		srv.mu.Unlock()
	}
}

func (c *client) processPong() {
	c.traceInOp("PONG", nil)
	c.mu.Lock()
	c.pout = 0
	c.mu.Unlock()
}

func (c *client) processMsgArgs(arg []byte) error {
	if c.trace {
		c.traceInOp("MSG", arg)
	}

	// Unroll splitArgs to avoid runtime/heap issues
	a := [MAX_MSG_ARGS][]byte{}
	args := a[:0]
	start := -1
	for i, b := range arg {
		switch b {
		case ' ', '\t', '\r', '\n':
			if start >= 0 {
				args = append(args, arg[start:i])
				start = -1
			}
		default:
			if start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		args = append(args, arg[start:])
	}

	switch len(args) {
	case 3:
		c.pa.reply = nil
		c.pa.szb = args[2]
		c.pa.size = parseSize(args[2])
	case 4:
		c.pa.reply = args[2]
		c.pa.szb = args[3]
		c.pa.size = parseSize(args[3])
	default:
		return fmt.Errorf("processMsgArgs Parse Error: '%s'", arg)
	}
	if c.pa.size < 0 {
		return fmt.Errorf("processMsgArgs Bad or Missing Size: '%s'", arg)
	}

	// Common ones processed after check for arg length
	c.pa.subject = args[0]
	c.pa.sid = args[1]

	return nil
}

func (c *client) processPub(arg []byte) error {
	if c.trace {
		c.traceInOp("PUB", arg)
	}

	// Unroll splitArgs to avoid runtime/heap issues
	a := [MAX_PUB_ARGS][]byte{}
	args := a[:0]
	start := -1
	for i, b := range arg {
		switch b {
		case ' ', '\t':
			if start >= 0 {
				args = append(args, arg[start:i])
				start = -1
			}
		default:
			if start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		args = append(args, arg[start:])
	}

	switch len(args) {
	case 2:
		c.pa.subject = args[0]
		c.pa.reply = nil
		c.pa.size = parseSize(args[1])
		c.pa.szb = args[1]
	case 3:
		c.pa.subject = args[0]
		c.pa.reply = args[1]
		c.pa.size = parseSize(args[2])
		c.pa.szb = args[2]
	default:
		return fmt.Errorf("processPub Parse Error: '%s'", arg)
	}
	if c.pa.size < 0 {
		return fmt.Errorf("processPub Bad or Missing Size: '%s'", arg)
	}
	maxPayload := atomic.LoadInt64(&c.mpay)
	if maxPayload > 0 && int64(c.pa.size) > maxPayload {
		c.maxPayloadViolation(c.pa.size, maxPayload)
		return ErrMaxPayload
	}

	if c.opts.Pedantic && !IsValidLiteralSubject(string(c.pa.subject)) {
		c.sendErr("Invalid Subject")
	}
	return nil
}

func splitArg(arg []byte) [][]byte {
	a := [MAX_MSG_ARGS][]byte{}
	args := a[:0]
	start := -1
	for i, b := range arg {
		switch b {
		case ' ', '\t', '\r', '\n':
			if start >= 0 {
				args = append(args, arg[start:i])
				start = -1
			}
		default:
			if start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		args = append(args, arg[start:])
	}
	return args
}

func (c *client) processSub(argo []byte) (err error) {
	c.traceInOp("SUB", argo)

	// Indicate activity.
	c.cache.subs++

	// Copy so we do not reference a potentially large buffer
	arg := make([]byte, len(argo))
	copy(arg, argo)
	args := splitArg(arg)
	sub := &subscription{client: c}
	switch len(args) {
	case 2:
		sub.subject = args[0]
		sub.queue = nil
		sub.sid = args[1]
	case 3:
		sub.subject = args[0]
		sub.queue = args[1]
		sub.sid = args[2]
	default:
		return fmt.Errorf("processSub Parse Error: '%s'", arg)
	}

	shouldForward := false

	c.mu.Lock()
	if c.nc == nil {
		c.mu.Unlock()
		return nil
	}

	// Check permissions if applicable.
	if !c.canSubscribe(sub.subject) {
		c.mu.Unlock()
		c.sendErr(fmt.Sprintf("Permissions Violation for Subscription to %q", sub.subject))
		c.Errorf("Subscription Violation - User %q, Subject %q, SID %s",
			c.opts.Username, sub.subject, sub.sid)
		return nil
	}

	// We can have two SUB protocols coming from a route due to some
	// race conditions. We should make sure that we process only one.
	sid := string(sub.sid)
	if c.subs[sid] == nil {
		c.subs[sid] = sub
		if c.srv != nil {
			err = c.srv.sl.Insert(sub)
			if err != nil {
				delete(c.subs, sid)
			} else {
				shouldForward = c.typ != ROUTER
			}
		}
	}
	c.mu.Unlock()
	if err != nil {
		c.sendErr("Invalid Subject")
		return nil
	} else if c.opts.Verbose {
		c.sendOK()
	}
	if shouldForward {
		c.srv.broadcastSubscribe(sub)
	}

	return nil
}

// canSubscribe determines if the client is authorized to subscribe to the
// given subject. Assumes caller is holding lock.
func (c *client) canSubscribe(sub []byte) bool {
	if c.perms == nil {
		return true
	}
	return len(c.perms.sub.Match(string(sub)).psubs) > 0
}

func (c *client) unsubscribe(sub *subscription) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sub.max > 0 && sub.nm < sub.max {
		c.Debugf(
			"Deferring actual UNSUB(%s): %d max, %d received\n",
			string(sub.subject), sub.max, sub.nm)
		return
	}
	c.traceOp("<-> %s", "DELSUB", sub.sid)
	delete(c.subs, string(sub.sid))
	if c.srv != nil {
		c.srv.sl.Remove(sub)
	}
}

func (c *client) processUnsub(arg []byte) error {
	c.traceInOp("UNSUB", arg)
	args := splitArg(arg)
	var sid []byte
	max := -1

	switch len(args) {
	case 1:
		sid = args[0]
	case 2:
		sid = args[0]
		max = parseSize(args[1])
	default:
		return fmt.Errorf("processUnsub Parse Error: '%s'", arg)
	}

	// Indicate activity.
	c.cache.subs += 1

	var sub *subscription

	unsub := false
	shouldForward := false
	ok := false

	c.mu.Lock()
	if sub, ok = c.subs[string(sid)]; ok {
		if max > 0 {
			sub.max = int64(max)
		} else {
			// Clear it here to override
			sub.max = 0
		}
		unsub = true
		shouldForward = c.typ != ROUTER && c.srv != nil
	}
	c.mu.Unlock()

	if unsub {
		c.unsubscribe(sub)
	}
	if shouldForward {
		c.srv.broadcastUnSubscribe(sub)
	}
	if c.opts.Verbose {
		c.sendOK()
	}

	return nil
}

func (c *client) msgHeader(mh []byte, sub *subscription) []byte {
	mh = append(mh, sub.sid...)
	mh = append(mh, ' ')
	if c.pa.reply != nil {
		mh = append(mh, c.pa.reply...)
		mh = append(mh, ' ')
	}
	mh = append(mh, c.pa.szb...)
	mh = append(mh, "\r\n"...)
	return mh
}

// Used to treat maps as efficient set
var needFlush = struct{}{}
var routeSeen = struct{}{}

func (c *client) deliverMsg(sub *subscription, mh, msg []byte) bool {
	if sub.client == nil {
		return false
	}
	client := sub.client
	client.mu.Lock()
	srv := client.srv

	sub.nm++
	// Check if we should auto-unsubscribe.
	if sub.max > 0 {
		// For routing..
		shouldForward := client.typ != ROUTER && client.srv != nil
		// If we are at the exact number, unsubscribe but
		// still process the message in hand, otherwise
		// unsubscribe and drop message on the floor.
		if sub.nm == sub.max {
			c.Debugf("Auto-unsubscribe limit of %d reached for sid '%s'\n", sub.max, string(sub.sid))
			// Due to defer, reverse the code order so that execution
			// is consistent with other cases where we unsubscribe.
			if shouldForward {
				defer srv.broadcastUnSubscribe(sub)
			}
			defer client.unsubscribe(sub)
		} else if sub.nm > sub.max {
			c.Debugf("Auto-unsubscribe limit [%d] exceeded\n", sub.max)
			client.mu.Unlock()
			client.unsubscribe(sub)
			if shouldForward {
				srv.broadcastUnSubscribe(sub)
			}
			return false
		}
	}

	// Check for closed connection
	if client.nc == nil {
		client.mu.Unlock()
		return false
	}

	// Update statistics

	// The msg includes the CR_LF, so pull back out for accounting.
	msgSize := int64(len(msg) - LEN_CR_LF)

	// No atomic needed since accessed under client lock.
	// Monitor is reading those also under client's lock.
	client.outMsgs++
	client.outBytes += msgSize

	atomic.AddInt64(&srv.outMsgs, 1)
	atomic.AddInt64(&srv.outBytes, msgSize)

	// Queue to outbound buffer
	client.queueOutbound(mh)
	client.queueOutbound(msg)

	// Check outbound threshold and queue IO flush if needed.
	if client.out.pb > maxBufSize*2 {
		client.flushSignal()
	}

	if c.trace {
		client.traceOutOp(string(mh[:len(mh)-LEN_CR_LF]), nil)
	}

	// Increment the flush pending signals if we are setting for the first time.
	if _, ok := c.pcd[client]; !ok {
		client.out.fps++
	}
	client.mu.Unlock()

	// Remember for when we return to the top of the loop.
	c.pcd[client] = needFlush

	return true
}

// pruneCache will prune the cache via randomly
// deleting items. Doing so pruneSize items at a time.
func (c *client) prunePubPermsCache() {
	r := 0
	for subject := range c.perms.pcache {
		delete(c.perms.pcache, subject)
		if r++; r > pruneSize {
			break
		}
	}
}

// pubAllowed checks on publish permissioning.
func (c *client) pubAllowed() bool {
	// Disallow publish to _SYS.>, these are reserved for internals.
	if c.pa.subject[0] == '_' && len(c.pa.subject) > 4 &&
		c.pa.subject[1] == 'S' && c.pa.subject[2] == 'Y' &&
		c.pa.subject[3] == 'S' && c.pa.subject[4] == '.' {
		c.pubPermissionViolation(c.pa.subject)
		return false
	}

	if c.perms == nil {
		return true
	}

	// Check if published subject is allowed if we have permissions in place.
	allowed, ok := c.perms.pcache[string(c.pa.subject)]
	if ok {
		if !allowed {
			c.pubPermissionViolation(c.pa.subject)
		}
		return allowed
	}
	// Cache miss
	r := c.perms.pub.Match(string(c.pa.subject))
	allowed = len(r.psubs) != 0
	if !allowed {
		c.pubPermissionViolation(c.pa.subject)
		c.perms.pcache[string(c.pa.subject)] = false
	} else {
		c.perms.pcache[string(c.pa.subject)] = true
	}
	// Prune if needed.

	if len(c.perms.pcache) > maxPermCacheSize {
		c.prunePubPermsCache()
	}
	return allowed
}

// processMsg is called to process an inbound msg from a client.
func (c *client) processMsg(msg []byte) {
	// Snapshot server.
	srv := c.srv

	// Update statistics
	// The msg includes the CR_LF, so pull back out for accounting.
	c.cache.inMsgs += 1
	c.cache.inBytes += len(msg) - LEN_CR_LF

	if c.trace {
		c.traceMsg(msg)
	}

	// Check pub permissions
	if !c.pubAllowed() {
		return
	}

	if c.opts.Verbose {
		c.sendOK()
	}

	// Mostly under testing scenarios.
	if srv == nil {
		return
	}

	var r *SublistResult
	var ok bool

	genid := atomic.LoadUint64(&srv.sl.genid)

	if genid == c.cache.genid && c.cache.results != nil {
		r, ok = c.cache.results[string(c.pa.subject)]
	} else {
		// reset our L1 completely.
		c.cache.results = make(map[string]*SublistResult)
		c.cache.genid = genid
	}

	if !ok {
		subject := string(c.pa.subject)
		r = srv.sl.Match(subject)
		c.cache.results[subject] = r
		// Prune the results cache. Keeps us from unbounded growth.
		if len(c.cache.results) > maxResultCacheSize {
			n := 0
			for subject := range c.cache.results {
				delete(c.cache.results, subject)
				if n++; n > pruneSize {
					break
				}
			}
		}
	}

	// This is the fanout scale.
	fanout := len(r.psubs) + len(r.qsubs)

	// Check for no interest, short circuit if so.
	if fanout == 0 {
		return
	}

	// Check for pedantic and bad subject.
	if c.opts.Pedantic && !IsValidLiteralSubject(string(c.pa.subject)) {
		return
	}

	// Scratch buffer..
	msgh := c.msgb[:len(msgHeadProto)]

	// msg header
	msgh = append(msgh, c.pa.subject...)
	msgh = append(msgh, ' ')
	si := len(msgh)

	isRoute := c.typ == ROUTER
	isRouteQsub := false

	// If we are a route and we have a queue subscription, deliver direct
	// since they are sent direct via L2 semantics. If the match is a queue
	// subscription, we will return from here regardless if we find a sub.
	if isRoute {
		isQueue, sub, err := srv.routeSidQueueSubscriber(c.pa.sid)
		if isQueue {
			// We got an invalid QRSID, so stop here
			if err != nil {
				c.Errorf("Unable to deliver messaage: %v", err)
				return
			}
			if sub != nil {
				mh := c.msgHeader(msgh[:si], sub)
				if c.deliverMsg(sub, mh, msg) {
					return
				}
			}
			isRouteQsub = true
			// At this point we know fo sure that it's a queue subscription and
			// we didn't make a delivery attempt, because either a subscriber limit
			// was exceeded or a subscription is already gone.
			// So, let the code below find yet another matching subscription.
			// We are at risk that a message might go back and forth between routes
			// during these attempts, but at the end it shall either be delivered
			// (at most once) or dropped.
		}
	}

	// Don't process normal subscriptions in case of a queue subscription resend.
	// Otherwise, we'd end up with potentially delivering the same message twice.
	if !isRouteQsub {
		// Used to only send normal subscriptions once across a given route.
		var rmap map[string]struct{}

		// Loop over all normal subscriptions that match.
		for _, sub := range r.psubs {
			// Check if this is a send to a ROUTER, make sure we only send it
			// once. The other side will handle the appropriate re-processing
			// and fan-out. Also enforce 1-Hop semantics, so no routing to another.
			if sub.client.typ == ROUTER {
				// Skip if sourced from a ROUTER and going to another ROUTER.
				// This is 1-Hop semantics for ROUTERs.
				if isRoute {
					continue
				}
				// Check to see if we have already sent it here.
				if rmap == nil {
					rmap = make(map[string]struct{}, srv.numRoutes())
				}
				sub.client.mu.Lock()
				if sub.client.nc == nil || sub.client.route == nil ||
					sub.client.route.remoteID == "" {
					c.Debugf("Bad or Missing ROUTER Identity, not processing msg")
					sub.client.mu.Unlock()
					continue
				}
				if _, ok := rmap[sub.client.route.remoteID]; ok {
					c.Debugf("Ignoring route, already processed")
					sub.client.mu.Unlock()
					continue
				}
				rmap[sub.client.route.remoteID] = routeSeen
				sub.client.mu.Unlock()
			}
			// Normal delivery
			mh := c.msgHeader(msgh[:si], sub)
			c.deliverMsg(sub, mh, msg)
		}
	}

	// Now process any queue subs we have if not a route...
	// or if we did not make a delivery attempt yet.
	if isRouteQsub || !isRoute {
		// Check to see if we have our own rand yet. Global rand
		// has contention with lots of clients, etc.
		if c.cache.prand == nil {
			c.cache.prand = rand.New(rand.NewSource(time.Now().UnixNano()))
		}
		// Process queue subs
		for i := 0; i < len(r.qsubs); i++ {
			qsubs := r.qsubs[i]
			// Find a subscription that is able to deliver this message
			// starting at a random index.
			startIndex := c.cache.prand.Intn(len(qsubs))
			for i := 0; i < len(qsubs); i++ {
				index := (startIndex + i) % len(qsubs)
				sub := qsubs[index]
				if sub != nil {
					mh := c.msgHeader(msgh[:si], sub)
					if c.deliverMsg(sub, mh, msg) {
						break
					}
				}
			}
		}
	}
}

func (c *client) pubPermissionViolation(subject []byte) {
	c.sendErr(fmt.Sprintf("Permissions Violation for Publish to %q", subject))
	c.Errorf("Publish Violation - User %q, Subject %q", c.opts.Username, subject)
}

func (c *client) processPingTimer() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ptmr = nil
	// Check if connection is still opened
	if c.nc == nil {
		return
	}

	c.Debugf("%s Ping Timer", c.typeString())

	// Check for violation
	c.pout++
	if c.pout > c.srv.getOpts().MaxPingsOut {
		c.Debugf("Stale Client Connection - Closing")
		c.sendProto([]byte(fmt.Sprintf("-ERR '%s'\r\n", "Stale Connection")), true)
		c.clearConnection()
		return
	}

	c.traceOutOp("PING", nil)

	// Send PING
	c.sendProto([]byte("PING\r\n"), true)
	// Reset to fire again.
	c.setPingTimer()
}

// Lock should be held
func (c *client) setPingTimer() {
	if c.srv == nil {
		return
	}
	d := c.srv.getOpts().PingInterval
	c.ptmr = time.AfterFunc(d, c.processPingTimer)
}

// Lock should be held
func (c *client) clearPingTimer() {
	if c.ptmr == nil {
		return
	}
	c.ptmr.Stop()
	c.ptmr = nil
}

// Lock should be held
func (c *client) setAuthTimer(d time.Duration) {
	c.atmr = time.AfterFunc(d, func() { c.authTimeout() })
}

// Lock should be held
func (c *client) clearAuthTimer() bool {
	if c.atmr == nil {
		return true
	}
	stopped := c.atmr.Stop()
	c.atmr = nil
	return stopped
}

func (c *client) isAuthTimerSet() bool {
	c.mu.Lock()
	isSet := c.atmr != nil
	c.mu.Unlock()
	return isSet
}

// Lock should be held
func (c *client) clearConnection() {
	if c.flags.isSet(clearConnection) {
		return
	}
	c.flags.set(clearConnection)
	nc := c.nc
	if nc == nil || c.srv == nil {
		return
	}
	// Flush any pending.
	c.flushOutbound()

	// Clear outbound here.
	c.out.sg.Broadcast()

	// With TLS, Close() is sending an alert (that is doing a write).
	// Need to set a deadline otherwise the server could block there
	// if the peer is not reading from socket.
	if c.flags.isSet(handshakeComplete) {
		nc.SetWriteDeadline(time.Now().Add(c.srv.getOpts().WriteDeadline))
	}
	nc.Close()
	// Do this always to also kick out any IO writes.
	nc.SetWriteDeadline(time.Time{})
	//fmt.Printf("MAX FT was %v\n", c.out.mft)
}

func (c *client) typeString() string {
	switch c.typ {
	case CLIENT:
		return "Client"
	case ROUTER:
		return "Router"
	}
	return "Unknown Type"
}

func (c *client) closeConnection() {
	c.mu.Lock()
	if c.nc == nil {
		c.mu.Unlock()
		return
	}

	c.Debugf("%s connection closed", c.typeString())

	c.clearAuthTimer()
	c.clearPingTimer()
	c.clearConnection()
	c.nc = nil

	// Snapshot for use.
	subs := make([]*subscription, 0, len(c.subs))
	for _, sub := range c.subs {
		// Auto-unsubscribe subscriptions must be unsubscribed forcibly.
		sub.max = 0
		subs = append(subs, sub)
	}
	srv := c.srv

	var (
		routeClosed   bool
		retryImplicit bool
		connectURLs   []string
	)
	if c.route != nil {
		routeClosed = c.route.closed
		if !routeClosed {
			retryImplicit = c.route.retry
		}
		connectURLs = c.route.connectURLs
	}

	c.mu.Unlock()

	if srv != nil {
		// This is a route that disconnected...
		if len(connectURLs) > 0 {
			// Unless disabled, possibly update the server's INFO protcol
			// and send to clients that know how to handle async INFOs.
			if !srv.getOpts().Cluster.NoAdvertise {
				srv.removeClientConnectURLsAndSendINFOToClients(connectURLs)
			}
		}

		// Unregister
		srv.removeClient(c)

		// Remove clients subscriptions.
		srv.sl.RemoveBatch(subs)
		if c.typ != ROUTER {
			for _, sub := range subs {
				// Forward on unsubscribes if we are not
				// a router ourselves.
				srv.broadcastUnSubscribe(sub)
			}
		}
	}

	// Don't reconnect routes that are being closed.
	if routeClosed {
		return
	}

	// Check for a solicited route. If it was, start up a reconnect unless
	// we are already connected to the other end.
	if c.isSolicitedRoute() || retryImplicit {
		// Capture these under lock
		c.mu.Lock()
		rid := c.route.remoteID
		rtype := c.route.routeType
		rurl := c.route.url
		c.mu.Unlock()

		srv.mu.Lock()
		defer srv.mu.Unlock()

		// It is possible that the server is being shutdown.
		// If so, don't try to reconnect
		if !srv.running {
			return
		}

		if rid != "" && srv.remotes[rid] != nil {
			c.srv.Debugf("Not attempting reconnect for solicited route, already connected to \"%s\"", rid)
			return
		} else if rid == srv.info.ID {
			c.srv.Debugf("Detected route to self, ignoring \"%s\"", rurl)
			return
		} else if rtype != Implicit || retryImplicit {
			c.srv.Debugf("Attempting reconnect for solicited route \"%s\"", rurl)
			// Keep track of this go-routine so we can wait for it on
			// server shutdown.
			srv.startGoRoutine(func() { srv.reConnectToRoute(rurl, rtype) })
		}
	}
}

// If the client is a route connection, sets the `closed` flag to true
// to prevent any reconnecting attempt when c.closeConnection() is called.
func (c *client) setRouteNoReconnectOnClose() {
	c.mu.Lock()
	if c.route != nil {
		c.route.closed = true
	}
	c.mu.Unlock()
}

// Logging functionality scoped to a client or route.

func (c *client) Errorf(format string, v ...interface{}) {
	format = fmt.Sprintf("%s - %s", c, format)
	c.srv.Errorf(format, v...)
}

func (c *client) Debugf(format string, v ...interface{}) {
	format = fmt.Sprintf("%s - %s", c, format)
	c.srv.Debugf(format, v...)
}

func (c *client) Noticef(format string, v ...interface{}) {
	format = fmt.Sprintf("%s - %s", c, format)
	c.srv.Noticef(format, v...)
}

func (c *client) Tracef(format string, v ...interface{}) {
	format = fmt.Sprintf("%s - %s", c, format)
	c.srv.Tracef(format, v...)
}
