// Copyright 2019 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package discover

import (
	"bytes"
	"crypto/ecdsa"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/p2p/netutil"
)

const (
	lookupRequestLimit      = 3  // max requests against a single node during lookup
	findnodeResultLimit     = 15 // applies in FINDNODE handler
	totalNodesResponseLimit = 5  // applies in waitForNodes
	nodesResponseItemLimit  = 3  // applies in sendNodes
)

// codecV5 is implemented by wireCodec (and testCodec).
//
// The UDPv5 transport is split into two objects: the codec object deals with
// encoding/decoding and with the handshake; the UDPv5 object handles higher-level concerns.
type codecV5 interface {
	// encode encodes a packet. The 'challenge' parameter is non-nil for calls which got a
	// WHOAREYOU response.
	encode(fromID enode.ID, fromAddr *net.UDPAddr, p packetV5, challenge *whoareyouV5) (enc []byte, authTag []byte, err error)
	// decode decodes a packet. It returns an *unknownV5 packet if decryption fails.
	// The fromNode return value is non-nil when the input contains a handshake response.
	decode(input []byte, fromAddr *net.UDPAddr) (fromID enode.ID, fromNode *enode.Node, p packetV5, err error)
}

// packetV5 is implemented by all discv5 packet type structs.
type packetV5 interface {
	// These methods provide information and set the request ID.
	name() string
	kind() byte
	setreqid([]byte)
	// handle should perform the appropriate action to handle the packet, i.e. this is the
	// place to send the response.
	handle(t *UDPv5, fromID enode.ID, fromAddr *net.UDPAddr)
}

// UDPv5 is the implementation of protocol version 5.
type UDPv5 struct {
	// static fields
	conn         UDPConn
	tab          *Table
	topictab     *topicTable
	ticketstore  *ticketStore
	netrestrict  *netutil.Netlist
	priv         *ecdsa.PrivateKey
	localNode    *enode.LocalNode
	db           *enode.DB
	log          log.Logger
	clock        mclock.Clock
	validSchemes enr.IdentityScheme

	// channels into dispatch
	packetInCh    chan ReadPacket
	callCh        chan *callV5
	callDoneCh    chan *callV5
	respTimeoutCh chan *callTimeout

	// state of dispatch
	codec            codecV5
	activeCallByNode map[enode.ID]*callV5
	activeCallByAuth map[string]*callV5
	callQueue        map[enode.ID][]*callV5

	// shutdown stuff
	closing   chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// callV5 represents a remote procedure call against another node.
type callV5 struct {
	node         *enode.Node
	packet       packetV5
	responseType byte // expected packet type of response
	reqid        []byte
	ch           chan packetV5 // responses sent here
	err          chan error    // errors sent here
	// Valid for active calls only:
	authTag        []byte
	handshakeCount int
	challenge      *whoareyouV5
	timeout        mclock.Event
}

// callTimeout is the response timeout event of a call.
type callTimeout struct {
	c     *callV5
	timer mclock.Event
}

// ListenV5 listens on the given connection.
func ListenV5(conn UDPConn, ln *enode.LocalNode, cfg Config) (*UDPv5, error) {
	t, err := newUDPv5(conn, ln, cfg)
	if err != nil {
		return nil, err
	}
	go t.tab.loop()
	t.wg.Add(2)
	go t.readLoop()
	go t.dispatch()
	return t, nil
}

// newUDPv5 creates a UDPv5 transport, but doesn't start any goroutines.
func newUDPv5(conn UDPConn, ln *enode.LocalNode, cfg Config) (*UDPv5, error) {
	cfg = cfg.withDefaults()
	t := &UDPv5{
		// static fields
		conn:         conn,
		localNode:    ln,
		db:           ln.Database(),
		netrestrict:  cfg.NetRestrict,
		priv:         cfg.PrivateKey,
		topictab:     newTopicTable(ln),
		ticketstore:  newTicketStore(),
		log:          cfg.Log,
		validSchemes: cfg.ValidSchemes,
		clock:        cfg.Clock,
		// channels into dispatch
		packetInCh:    make(chan ReadPacket, 1),
		callCh:        make(chan *callV5),
		callDoneCh:    make(chan *callV5),
		respTimeoutCh: make(chan *callTimeout),
		closing:       make(chan struct{}),
		// state of dispatch
		codec:            newWireCodec(ln, cfg.PrivateKey, cfg.Clock),
		activeCallByNode: make(map[enode.ID]*callV5),
		activeCallByAuth: make(map[string]*callV5),
		callQueue:        make(map[enode.ID][]*callV5),
	}
	tab, err := newTable(t, t.db, cfg.Bootnodes, cfg.Log)
	if err != nil {
		return nil, err
	}
	t.tab = tab
	return t, nil
}

// Self returns the local node record.
func (t *UDPv5) Self() *enode.Node {
	return t.localNode.Node()
}

// Close shuts down packet processing.
func (t *UDPv5) Close() {
	t.closeOnce.Do(func() {
		close(t.closing)
		t.conn.Close()
		t.wg.Wait()
		t.tab.close()
	})
}

// ReadRandomNodes reads random nodes from the local table.
func (t *UDPv5) ReadRandomNodes(buf []*enode.Node) int {
	return t.tab.ReadRandomNodes(buf)
}

// Ping sends a ping message to the given node.
func (t *UDPv5) Ping(n *enode.Node) error {
	_, err := t.ping(n)
	return err
}

// Resolve searches for a specific node with the given ID and tries to get the most recent
// version of the node record for it. It returns n if the node could not be resolved.
func (t *UDPv5) Resolve(n *enode.Node) *enode.Node {
	if intable := t.tab.getNode(n.ID()); intable != nil && intable.Seq() > n.Seq() {
		n = intable
	}
	// Try asking directly. This works if the node is still responding on the endpoint we have.
	if resp, err := t.RequestENR(n); err == nil {
		return resp
	}
	// Otherwise do a network lookup.
	result := t.Lookup(n.ID())
	for _, rn := range result {
		if rn.ID() == n.ID() && rn.Seq() > n.Seq() {
			return rn
		}
	}
	return n
}

// LookupRandom finds random nodes in the network.
func (t *UDPv5) LookupRandom() []*enode.Node {
	if t.tab.len() == 0 {
		// All nodes were dropped, refresh. The very first query will hit this
		// case and run the bootstrapping logic.
		<-t.tab.refresh()
	}
	return t.lookupRandom()
}

// lookupRandom looks up a random target.
// This is needed to satisfy the transport interface.
func (t *UDPv5) lookupRandom() []*enode.Node {
	var target enode.ID
	crand.Read(target[:])
	return t.Lookup(target)
}

// lookupSelf looks up our own node ID.
// This is needed to satisfy the transport interface.
func (t *UDPv5) lookupSelf() []*enode.Node {
	return t.Lookup(t.Self().ID())
}

// Lookup performs a recursive lookup for the given target.
// It returns the closest nodes to target.
func (t *UDPv5) Lookup(target enode.ID) []*enode.Node {
	var (
		asked          = make(map[enode.ID]bool)
		seen           = make(map[enode.ID]bool)
		response       = make(chan []*node, alpha)
		pendingQueries = 0
		result         *nodesByDistance
	)
	// Don't query further if we hit ourself.
	// Unlikely to happen often in practice.
	asked[t.Self().ID()] = true

	// Generate the initial result set.
	t.tab.mutex.Lock()
	result = t.tab.closest(target, bucketSize, false)
	t.tab.mutex.Unlock()

	for {
		// Ask the closest nodes we haven't asked yet.
		for i := 0; i < len(result.entries) && pendingQueries < alpha; i++ {
			n := result.entries[i]
			if !asked[n.ID()] {
				asked[n.ID()] = true
				pendingQueries++
				go t.lookupWorker(n, target, response)
			}
		}
		if pendingQueries == 0 {
			// We have asked all closest nodes, stop the search.
			break
		}
		select {
		case nodes := <-response:
			for _, n := range nodes {
				if n != nil && !seen[n.ID()] {
					seen[n.ID()] = true
					result.push(n, bucketSize)
				}
			}
		case <-t.closing:
			return nil // Shutdown, no need to continue.
		}
		pendingQueries--
	}
	return unwrapNodes(result.entries)
}

// lookupWorker performs FINDNODE calls against a single node during lookup.
func (t *UDPv5) lookupWorker(destNode *node, target enode.ID, response chan<- []*node) {
	var (
		dists = lookupDistances(target, destNode.ID())
		nodes = nodesByDistance{target: target}
	)
	for i := 0; i < lookupRequestLimit && len(nodes.entries) < findnodeResultLimit; i++ {
		fails := t.db.FindFailsV5(destNode.ID())
		r, err := t.findnode(unwrapNode(destNode), dists[i])
		if err == errClosed {
			// Avoid recording failures on shutdown.
			nodes.entries = nil
			break
		}
		if len(r) == 0 {
			// The query failed. Record the failure and drop the node if it fails repeatedly.
			fails++
			t.log.Trace("FINDNODE/v5 call found no useful nodes", "id", destNode.ID(), "d", dists[i], "failcount", fails, "err", err)
			if fails >= maxFindnodeFailures {
				t.log.Trace("Too many findnode failures, dropping", "id", destNode.ID(), "failcount", fails)
				t.tab.delete(destNode)
				break
			}
		} else if fails > 0 {
			t.db.UpdateFindFailsV5(destNode.ID(), fails-1)
		}
		for _, n := range r {
			if n.ID() != t.Self().ID() {
				nodes.push(wrapNode(n), findnodeResultLimit)
			}
		}
	}

	// Add all result nodes to table. Some of them might not be alive anymore, but we'll
	// just remove those again during revalidation.
	for _, n := range nodes.entries {
		t.tab.addSeenNode(n)
	}
	response <- nodes.entries
}

// lookupDistances computes the distance parameter for FINDNODE calls to dest.
// It chooses distances adjacent to logdist(target, dest), e.g. for a target
// with logdist(target, dest) = 255 the result is [255, 256, 254].
func lookupDistances(target, dest enode.ID) (dists []int) {
	td := enode.LogDist(target, dest)
	dists = append(dists, td)
	for i := 1; len(dists) < lookupRequestLimit; i++ {
		if td+i < 256 {
			dists = append(dists, td+i)
		}
		if td-i > 0 {
			dists = append(dists, td-i)
		}
	}
	return dists
}

// ping calls PING on a node and waits for a PONG response.
func (t *UDPv5) ping(n *enode.Node) (uint64, error) {
	resp := t.call(n, p_pongV5, &pingV5{ENRSeq: t.localNode.Node().Seq()})
	defer t.callDone(resp)
	select {
	case pong := <-resp.ch:
		return pong.(*pongV5).ENRSeq, nil
	case err := <-resp.err:
		return 0, err
	}
}

// requestENR requests n's record.
func (t *UDPv5) RequestENR(n *enode.Node) (*enode.Node, error) {
	nodes, err := t.findnode(n, 0)
	if err != nil {
		return nil, err
	}
	if len(nodes) != 1 {
		return nil, fmt.Errorf("%d nodes in response for distance zero", len(nodes))
	}
	return nodes[0], nil
}

// requestTicket calls REQUESTTICKET on a node and waits for a TICKET response.
func (t *UDPv5) requestTicket(n *enode.Node) ([]byte, error) {
	resp := t.call(n, p_ticketV5, &pingV5{})
	defer t.callDone(resp)
	select {
	case response := <-resp.ch:
		return response.(*ticketV5).Ticket, nil
	case err := <-resp.err:
		return nil, err
	}
}

// findnode calls FINDNODE on a node and waits for responses.
func (t *UDPv5) findnode(n *enode.Node, distance int) ([]*enode.Node, error) {
	resp := t.call(n, p_nodesV5, &findnodeV5{Distance: uint(distance)})
	return t.waitForNodes(resp, distance)
}

// topicQuery calls TOPICQUERY on a node and waits for responses.
func (t *UDPv5) topicQuery(n *enode.Node, topic Topic) ([]*enode.Node, error) {
	resp := t.call(n, p_nodesV5, &topicqueryV5{Topic: topic})
	return t.waitForNodes(resp, -1)
}

// waitForNodes waits for NODES responses to the given call.
func (t *UDPv5) waitForNodes(c *callV5, distance int) ([]*enode.Node, error) {
	defer t.callDone(c)

	var (
		nodes           []*enode.Node
		seen            = make(map[enode.ID]struct{})
		received, total = 0, -1
	)
	for {
		select {
		case responseP := <-c.ch:
			response := responseP.(*nodesV5)
			for _, record := range response.Nodes {
				node, err := t.verifyResponseNode(c, record, distance, seen)
				if err != nil {
					t.log.Debug("Invalid record in "+response.name(), "id", c.node.ID(), "err", err)
					continue
				}
				nodes = append(nodes, node)
			}
			if total == -1 {
				total = min(int(response.Total), totalNodesResponseLimit)
			}
			if received++; received == total {
				return nodes, nil
			}
		case err := <-c.err:
			return nodes, err
		}
	}
}

// verifyResponseNode checks validity of a record in a NODES response.
func (t *UDPv5) verifyResponseNode(c *callV5, r *enr.Record, distance int, seen map[enode.ID]struct{}) (*enode.Node, error) {
	node, err := enode.New(t.validSchemes, r)
	if err != nil {
		return nil, err
	}
	if err := netutil.CheckRelayIP(c.node.IP(), node.IP()); err != nil {
		return nil, err
	}
	if c.node.UDP() <= 1024 {
		return nil, errLowPort
	}
	if distance != -1 {
		if d := enode.LogDist(c.node.ID(), node.ID()); d != distance {
			return nil, fmt.Errorf("wrong distance %d", d)
		}
	}
	if _, ok := seen[node.ID()]; ok {
		return nil, fmt.Errorf("duplicate record")
	}
	seen[node.ID()] = struct{}{}
	return node, nil
}

// call sends the given call and sets up a handler for response packets (of type c.responseType).
// Responses are dispatched to the call's response channel.
func (t *UDPv5) call(node *enode.Node, responseType byte, packet packetV5) *callV5 {
	c := &callV5{
		node:         node,
		packet:       packet,
		responseType: responseType,
		reqid:        make([]byte, 8),
		ch:           make(chan packetV5, 1),
		err:          make(chan error, 1),
	}
	// Assign request ID.
	crand.Read(c.reqid)
	packet.setreqid(c.reqid)
	// Send call to dispatch.
	select {
	case t.callCh <- c:
	case <-t.closing:
		c.err <- errClosed
	}
	return c
}

// callDone tells dispatch that the active call is done.
func (t *UDPv5) callDone(c *callV5) {
	select {
	case t.callDoneCh <- c:
	case <-t.closing:
	}
}

// dispatch runs in its own goroutine, handles incoming packets and deals with calls.
//
// For any destination node there is at most one 'active call', stored in the t.activeCall*
// maps. A call is made active when it is sent. The active call can be answered by a
// matching response, in which case c.ch receives the response; or by timing out, in which case
// c.err receives the error. When the function that created the call signals the active
// call is done through callDone, the next call from the call queue is started.
//
// Calls may also be answered by a WHOAREYOU packet referencing the call packet's authTag.
// When that happens the call is simply re-sent to complete the handshake. We allow one
// handshake attempt per call.
func (t *UDPv5) dispatch() {
	defer t.wg.Done()

	for {
		select {
		case c := <-t.callCh:
			id := c.node.ID()
			t.callQueue[id] = append(t.callQueue[id], c)
			t.sendNextCall(id)

		case ct := <-t.respTimeoutCh:
			active := t.activeCallByNode[ct.c.node.ID()]
			if ct.c == active && ct.timer == active.timeout {
				ct.c.err <- errTimeout
			}

		case c := <-t.callDoneCh:
			id := c.node.ID()
			active := t.activeCallByNode[id]
			if active != c {
				panic("BUG: callDone for inactive call")
			}
			c.timeout.Cancel()
			delete(t.activeCallByAuth, string(c.authTag))
			delete(t.activeCallByNode, id)
			t.sendNextCall(id)

		case p := <-t.packetInCh:
			t.handlePacket(p.Data, p.Addr)

		case <-t.closing:
			for id, queue := range t.callQueue {
				for _, c := range queue {
					c.err <- errClosed
				}
				delete(t.callQueue, id)
			}
			for id, c := range t.activeCallByNode {
				c.err <- errClosed
				delete(t.activeCallByNode, id)
				delete(t.activeCallByAuth, string(c.authTag))
			}
			return
		}
	}
}

// startResponseTimeout sets the response timer for a call.
func (t *UDPv5) startResponseTimeout(c *callV5) {
	if c.timeout != nil {
		c.timeout.Cancel()
	}
	var (
		timer mclock.Event
		done  = make(chan struct{})
	)
	timer = t.clock.AfterFunc(respTimeout, func() {
		<-done
		select {
		case t.respTimeoutCh <- &callTimeout{c, timer}:
		case <-t.closing:
		}
	})
	c.timeout = timer
	close(done)
}

// sendNextCall sends the next call in the call queue if there is no active call.
func (t *UDPv5) sendNextCall(id enode.ID) {
	queue := t.callQueue[id]
	if len(queue) == 0 || t.activeCallByNode[id] != nil {
		return
	}
	t.activeCallByNode[id] = queue[0]
	t.sendCall(t.activeCallByNode[id])
	if len(queue) == 1 {
		delete(t.callQueue, id)
	} else {
		copy(queue, queue[1:])
		t.callQueue[id] = queue[:len(queue)-1]
	}
}

// sendCall encodes and sends a request packet to the call's recipient node.
// This performs a handshake if needed.
func (t *UDPv5) sendCall(c *callV5) {
	if len(c.authTag) > 0 {
		delete(t.activeCallByAuth, string(c.authTag))
	}
	addr := &net.UDPAddr{IP: c.node.IP(), Port: c.node.UDP()}
	newTag, _ := t.send(c.node.ID(), addr, c.packet, c.challenge)
	c.authTag = newTag
	t.activeCallByAuth[string(c.authTag)] = c
	t.startResponseTimeout(c)
}

// sendResponse sends a response packet to the given node.
// This doesn't trigger a handshake even if no keys are available.
func (t *UDPv5) sendResponse(toID enode.ID, toAddr *net.UDPAddr, packet packetV5) error {
	_, err := t.send(toID, toAddr, packet, nil)
	return err
}

// send sends a packet to the given node.
func (t *UDPv5) send(toID enode.ID, toAddr *net.UDPAddr, packet packetV5, c *whoareyouV5) ([]byte, error) {
	enc, authTag, err := t.codec.encode(toID, toAddr, packet, c)
	if err != nil {
		t.log.Warn(">> "+packet.name(), "id", toID, "addr", toAddr, "err", err)
		return authTag, err
	}
	_, err = t.conn.WriteToUDP(enc, toAddr)
	t.log.Trace(">> "+packet.name(), "id", toID, "addr", toAddr)
	return authTag, err
}

// readLoop runs in its own goroutine and reads packets from the network.
func (t *UDPv5) readLoop() {
	defer t.wg.Done()

	buf := make([]byte, maxPacketSize)
	for {
		nbytes, from, err := t.conn.ReadFromUDP(buf)
		if netutil.IsTemporaryError(err) {
			// Ignore temporary read errors.
			t.log.Debug("Temporary UDP read error", "err", err)
			continue
		} else if err != nil {
			// Shut down the loop for permament errors.
			if err != io.EOF {
				t.log.Debug("UDP read error", "err", err)
			}
			return
		}
		select {
		case t.packetInCh <- ReadPacket{Data: buf[:nbytes], Addr: from}:
		case <-t.closing:
			return
		}
	}
}

// handlePacket decodes and processes an incoming packet from the network.
func (t *UDPv5) handlePacket(rawpacket []byte, fromAddr *net.UDPAddr) error {
	fromID, fromNode, packet, err := t.codec.decode(rawpacket, fromAddr)
	if err != nil {
		t.log.Debug("Bad discv5 packet", "id", fromID, "addr", fromAddr, "err", err)
		return err
	}
	if fromNode != nil {
		t.tab.addSeenNode(wrapNode(fromNode))
	}
	// Call the packet handler.
	t.log.Trace("<< "+packet.name(), "id", fromID, "addr", fromAddr)
	packet.handle(t, fromID, fromAddr)
	return nil
}

// handleCallResponse dispatches a response packet to the call waiting for it.
func (t *UDPv5) handleCallResponse(fromID enode.ID, fromAddr *net.UDPAddr, reqid []byte, p packetV5) {
	ac := t.activeCallByNode[fromID]
	if ac == nil || !bytes.Equal(reqid, ac.reqid) {
		t.log.Debug(fmt.Sprintf("Unsolicited/late %s response", p.name()), "id", fromID, "addr", fromAddr)
		return
	}
	if !fromAddr.IP.Equal(ac.node.IP()) || fromAddr.Port != ac.node.UDP() {
		t.log.Debug(fmt.Sprintf("%s from wrong endpoint", p.name()), "id", fromID, "addr", fromAddr)
		return
	}
	if p.kind() != ac.responseType {
		t.log.Debug(fmt.Sprintf("Wrong disv5 response type %s", p.name()), "id", fromID, "addr", fromAddr)
		return
	}
	t.startResponseTimeout(ac)
	ac.ch <- p
}

// getNode looks for a node record in table and database.
func (t *UDPv5) getNode(id enode.ID) *enode.Node {
	if n := t.tab.getNode(id); n != nil {
		return n
	}
	if n := t.localNode.Database().Node(id); n != nil {
		return n
	}
	return nil
}

// UNKNOWN

func (p *unknownV5) name() string       { return "UNKNOWN/v5" }
func (p *unknownV5) kind() byte         { return p_unknownV5 }
func (p *unknownV5) setreqid(id []byte) {}

func (p *unknownV5) handle(t *UDPv5, fromID enode.ID, fromAddr *net.UDPAddr) {
	challenge := &whoareyouV5{AuthTag: p.AuthTag}
	crand.Read(challenge.IDNonce[:])
	if n := t.getNode(fromID); n != nil {
		challenge.node = n
		challenge.RecordSeq = n.Seq()
	}
	t.sendResponse(fromID, fromAddr, challenge)
}

// WHOAREYOU

func (p *whoareyouV5) name() string       { return "WHOAREYOU/v5" }
func (p *whoareyouV5) kind() byte         { return p_whoareyouV5 }
func (p *whoareyouV5) setreqid(id []byte) {}

func (p *whoareyouV5) handle(t *UDPv5, fromID enode.ID, fromAddr *net.UDPAddr) {
	c, err := p.matchWithCall(t, p.AuthTag)
	if err != nil {
		t.log.Debug("Invalid WHOAREYOU/v5", "id", fromID, "addr", fromAddr, "err", err)
		return
	}
	// Resend the call that was answered by WHOAREYOU.
	c.handshakeCount++
	c.challenge = p
	p.node = c.node
	t.sendCall(c)
}

var (
	errChallengeNoCall = errors.New("no matching call")
	errChallengeTwice  = errors.New("second handshake")
)

// matchWithCall checks whether the handshake attempt matches the active call.
func (p *whoareyouV5) matchWithCall(t *UDPv5, authTag []byte) (*callV5, error) {
	c := t.activeCallByAuth[string(authTag)]
	if c == nil {
		return nil, errChallengeNoCall
	}
	if c.handshakeCount > 0 {
		return nil, errChallengeTwice
	}
	return c, nil
}

// PING

func (p *pingV5) name() string       { return "PING/v5" }
func (p *pingV5) kind() byte         { return p_pingV5 }
func (p *pingV5) setreqid(id []byte) { p.ReqID = id }

func (p *pingV5) handle(t *UDPv5, fromID enode.ID, fromAddr *net.UDPAddr) {
	t.sendResponse(fromID, fromAddr, &pongV5{
		ReqID:  p.ReqID,
		ToIP:   fromAddr.IP,
		ToPort: uint16(fromAddr.Port),
		ENRSeq: t.localNode.Node().Seq(),
	})
}

// PONG

func (p *pongV5) name() string       { return "PONG/v5" }
func (p *pongV5) kind() byte         { return p_pongV5 }
func (p *pongV5) setreqid(id []byte) { p.ReqID = id }

func (p *pongV5) handle(t *UDPv5, fromID enode.ID, fromAddr *net.UDPAddr) {
	t.localNode.UDPEndpointStatement(fromAddr, &net.UDPAddr{IP: p.ToIP, Port: int(p.ToPort)})
	t.handleCallResponse(fromID, fromAddr, p.ReqID, p)
}

// FINDNODE

func (p *findnodeV5) name() string       { return "FINDNODE/v5" }
func (p *findnodeV5) kind() byte         { return p_findnodeV5 }
func (p *findnodeV5) setreqid(id []byte) { p.ReqID = id }

func (p *findnodeV5) handle(t *UDPv5, fromID enode.ID, fromAddr *net.UDPAddr) {
	if p.Distance == 0 {
		t.sendNodes(fromID, fromAddr, p.ReqID, []*enode.Node{t.Self()})
		return
	}
	if p.Distance > 256 {
		p.Distance = 256
	}
	// Get bucket entries.
	t.tab.mutex.Lock()
	nodes := unwrapNodes(t.tab.bucketAtDistance(int(p.Distance)).entries)
	t.tab.mutex.Unlock()
	if len(nodes) > findnodeResultLimit {
		nodes = nodes[:findnodeResultLimit]
	}
	t.sendNodes(fromID, fromAddr, p.ReqID, nodes)
}

// sendNodes sends the given records in one or more NODES packets.
func (t *UDPv5) sendNodes(toID enode.ID, toAddr *net.UDPAddr, reqid []byte, nodes []*enode.Node) {
	// TODO livenessChecks > 1
	// TODO CheckRelayIP
	total := uint8(math.Ceil(float64(len(nodes)) / 3))
	resp := &nodesV5{ReqID: reqid, Total: total, Nodes: make([]*enr.Record, 3)}
	sent := false
	for len(nodes) > 0 {
		items := min(nodesResponseItemLimit, len(nodes))
		resp.Nodes = resp.Nodes[:items]
		for i := 0; i < items; i++ {
			resp.Nodes[i] = nodes[i].Record()
		}
		t.sendResponse(toID, toAddr, resp)
		nodes = nodes[items:]
		sent = true
	}
	// Ensure at least one response is sent.
	if !sent {
		resp.Total = 1
		resp.Nodes = nil
		t.sendResponse(toID, toAddr, resp)
	}
}

// NODES

func (p *nodesV5) name() string       { return "NODES/v5" }
func (p *nodesV5) kind() byte         { return p_nodesV5 }
func (p *nodesV5) setreqid(id []byte) { p.ReqID = id }

func (p *nodesV5) handle(t *UDPv5, fromID enode.ID, fromAddr *net.UDPAddr) {
	t.handleCallResponse(fromID, fromAddr, p.ReqID, p)
}

// REQUESTTICKET

func (p *requestTicketV5) name() string       { return "REQUESTTICKET/v5" }
func (p *requestTicketV5) kind() byte         { return p_requestTicketV5 }
func (p *requestTicketV5) setreqid(id []byte) { p.ReqID = id }

func (p *requestTicketV5) handle(t *UDPv5, fromID enode.ID, fromAddr *net.UDPAddr) {
	t.sendResponse(fromID, fromAddr, &ticketV5{ReqID: p.ReqID})
}

// TICKET

func (p *ticketV5) name() string       { return "TICKET/v5" }
func (p *ticketV5) kind() byte         { return p_ticketV5 }
func (p *ticketV5) setreqid(id []byte) { p.ReqID = id }

func (p *ticketV5) handle(t *UDPv5, fromID enode.ID, fromAddr *net.UDPAddr) {
	t.handleCallResponse(fromID, fromAddr, p.ReqID, p)
}

// REGTOPIC

func (p *regtopicV5) name() string       { return "REGTOPIC/v5" }
func (p *regtopicV5) kind() byte         { return p_regtopicV5 }
func (p *regtopicV5) setreqid(id []byte) { p.ReqID = id }

func (p *regtopicV5) handle(t *UDPv5, fromID enode.ID, fromAddr *net.UDPAddr) {
	t.sendResponse(fromID, fromAddr, &regconfirmationV5{ReqID: p.ReqID, Registered: false})
}

// REGCONFIRMATION

func (p *regconfirmationV5) name() string       { return "REGCONFIRMATION/v5" }
func (p *regconfirmationV5) kind() byte         { return p_regconfirmationV5 }
func (p *regconfirmationV5) setreqid(id []byte) { p.ReqID = id }

func (p *regconfirmationV5) handle(t *UDPv5, fromID enode.ID, fromAddr *net.UDPAddr) {
	t.handleCallResponse(fromID, fromAddr, p.ReqID, p)
}

// TOPICQUERY

func (p *topicqueryV5) name() string       { return "TOPICQUERY/v5" }
func (p *topicqueryV5) kind() byte         { return p_topicqueryV5 }
func (p *topicqueryV5) setreqid(id []byte) { p.ReqID = id }

func (p *topicqueryV5) handle(t *UDPv5, fromID enode.ID, fromAddr *net.UDPAddr) {
	nodes := t.topictab.getEntries(p.Topic)
	t.sendNodes(fromID, fromAddr, p.ReqID, nodes)
}
