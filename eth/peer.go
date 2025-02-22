// Copyright 2015 The go-ethereum Authors
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

package eth

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/Venachain/Venachain/consensus"

	"github.com/Venachain/Venachain/common"
	"github.com/Venachain/Venachain/core/types"
	"github.com/Venachain/Venachain/p2p"
	"github.com/Venachain/Venachain/rlp"
	mapset "github.com/deckarep/golang-set"
)

var (
	errClosed            = errors.New("peer set is closed")
	errAlreadyRegistered = errors.New("peer is already registered")
	errNotRegistered     = errors.New("peer is not registered")
)

const (
	maxKnownTxs    = 32768 // Maximum transactions hashes to keep in the known list (prevent DOS)
	maxKnownBlocks = 1024  // Maximum block hashes to keep in the known list (prevent DOS)

	// maxQueuedTxs is the maximum number of transaction lists to queue up before
	// dropping broadcasts. This is a sensitive number as a transaction list might
	// contain a single transaction, or thousands.
	maxQueuedTxs = 128

	maxQueuedTxHashes = 128

	// maxQueuedProps is the maximum number of block propagations to queue up before
	// dropping broadcasts. There's not much point in queueing stale blocks, so a few
	// that might cover uncles should be enough.
	maxQueuedProps = 4

	maxQueuedPreBlock  = 4
	maxQueuedSignature = 4

	// maxQueuedAnns is the maximum number of block announcements to queue up before
	// dropping broadcasts. Similarly to block propagations, there's no point to queue
	// above some healthy uncle limit, so use that.
	maxQueuedAnns = 4

	handshakeTimeout = 5 * time.Second
)

// max is a helper function which returns the larger of the two given integers.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// PeerInfo represents a short summary of the Ethereum sub-protocol metadata known
// about a connected peer.
type PeerInfo struct {
	Version int      `json:"version"` // Ethereum protocol version negotiated
	BN      *big.Int `json:"number"`  // The block number of the peer's blockchain
	Head    string   `json:"head"`    // SHA3 hash of the peer's best owned block
}

// propEvent is a block propagation, waiting for its turn in the broadcast queue.
type propEvent struct {
	block *types.Block
}

type peer struct {
	id string

	*p2p.Peer
	rw p2p.MsgReadWriter

	version  int         // Protocol version negotiated
	forkDrop *time.Timer // Timed connection dropper if forks aren't validated in time

	head common.Hash
	bn   *big.Int
	lock sync.RWMutex

	knownTxs           mapset.Set                // Set of transaction hashes known to be known by this peer
	knownBlocks        mapset.Set                // Set of block hashes known to be known by this peer
	knownPrepareBlocks mapset.Set                // Set of prepareblock hashes known to be known by this peer
	queuedTxs          chan []*types.Transaction // Queue of transactions to broadcast to the peer
	queuedHashes       chan []common.Hash        // Queue of transaction hashes to broadcast to the peer
	queuedProps        chan *propEvent           // Queue of blocks to broadcast to the peer
	queuedAnns         chan *types.Block         // Queue of blocks to announce to the peer
	term               chan struct{}             // Termination channel to stop the broadcaster
	queuedPreBlock     chan *preBlockEvent
	types              int32 // remote node's types   consensus(1) / observer(0)
	replayParam        common.ReplayParam
}

func newPeer(version int, p *p2p.Peer, rw p2p.MsgReadWriter) *peer {
	return &peer{
		Peer:           p,
		rw:             rw,
		version:        version,
		id:             fmt.Sprintf("%x", p.ID().Bytes()[:8]),
		knownTxs:       mapset.NewSet(),
		knownBlocks:    mapset.NewSet(),
		queuedTxs:      make(chan []*types.Transaction, maxQueuedTxs),
		queuedHashes:   make(chan []common.Hash, maxQueuedTxHashes),
		queuedProps:    make(chan *propEvent, maxQueuedProps),
		queuedAnns:     make(chan *types.Block, maxQueuedAnns),
		term:           make(chan struct{}),
		queuedPreBlock: make(chan *preBlockEvent, maxQueuedPreBlock),
		types:          common.SysCfg.GetNodeTypes(p.ID().String()),
	}
}

// broadcast is a write loop that multiplexes block propagations, announcements
// and transaction broadcasts into the remote peer. The goal is to have an async
// writer that does not lock up node internals.
func (p *peer) broadcast(removePeer func(string)) {
	go func() {
		for {
			select {
			case prop := <-p.queuedProps:
				if err := p.SendNewBlock(prop.block); err != nil {
					p.Log().Error("Propagated block", "number", prop.block.Number(), "hash", prop.block.Hash(), "err", err)
					removePeer(p.id)
					return
				}
				p.Log().Trace("Propagated block", "number", prop.block.Number(), "hash", prop.block.Hash())

			case block := <-p.queuedAnns:
				if err := p.SendNewBlockHashes([]common.Hash{block.Hash()}, []uint64{block.NumberU64()}); err != nil {
					p.Log().Error("Announced block", "number", block.Number(), "hash", block.Hash(), "err", err)
					removePeer(p.id)
					return
				}
				p.Log().Trace("Announced block", "number", block.Number(), "hash", block.Hash())

			case prop := <-p.queuedPreBlock:
				if err := p.SendPrepareBlock(prop.block); err != nil {
					p.Log().Error("Propagated prepare block", "number", prop.block.Number(), "hash", prop.block.Hash(), "err", err)
					removePeer(p.id)
					return
				}
				p.Log().Trace("Propagated prepare block", "number", prop.block.Number(), "hash", prop.block.Hash())

			case <-p.term:
				return
			}
		}
	}()

	go func() {
		for {
			select {
			case txs := <-p.queuedTxs:
				if err := p.SendTransactions(txs); err != nil {
					p.Log().Error("Broadcast transactions err", "err", err)
					removePeer(p.id)
					return
				}
				p.Log().Trace("Broadcast transactions", "count", len(txs))

			case <-p.term:
				return
			}
		}
	}()

	go func() {
		for {
			select {
			case hashes := <-p.queuedHashes:
				if err := p.sendPooledTransactionHashes(hashes); err != nil {
					p.Log().Error("Broadcast transaction hashes error ", "err", err)
					removePeer(p.id)
					return
				}
				p.Log().Trace("Broadcast transaction hashes", "count", len(hashes))

			case <-p.term:
				return
			}
		}
	}()

}

// close signals the broadcast goroutine to terminate.
func (p *peer) close() {
	close(p.term)
}

func (p *peer) setTypes(types int32) {
	p.types = types
}

func (p *peer) IsConsensus() bool {
	return p.types == 1
}

// Info gathers and returns a collection of metadata known about a peer.
func (p *peer) Info() *PeerInfo {
	hash, bn := p.Head()

	return &PeerInfo{
		Version: p.version,
		BN:      bn,
		Head:    hash.Hex(),
	}
}

// Head retrieves a copy of the current head hash and total difficulty of the
// peer.
func (p *peer) Head() (hash common.Hash, bn *big.Int) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	copy(hash[:], p.head[:])
	return hash, new(big.Int).Set(p.bn)
}

// SetHead updates the head hash and total difficulty of the peer.
func (p *peer) SetHead(hash common.Hash, bn *big.Int) {
	p.lock.Lock()
	defer p.lock.Unlock()

	copy(p.head[:], hash[:])
	p.bn.Set(bn)
}

func (p *peer) SetReplayParam(param common.ReplayParam) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.replayParam = param
}

func (p *peer) GetReplayParam() common.ReplayParam {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.replayParam
}

// MarkBlock marks a block as known for the peer, ensuring that the block will
// never be propagated to this particular peer.
func (p *peer) MarkBlock(hash common.Hash) {
	// If we reached the memory allowance, drop a previously known block hash
	for p.knownBlocks.Cardinality() >= maxKnownBlocks {
		p.knownBlocks.Pop()
	}
	p.knownBlocks.Add(hash)
}

// MarkTransaction marks a transaction as known for the peer, ensuring that it
// will never be propagated to this particular peer.
func (p *peer) MarkTransaction(hash common.Hash) {
	// If we reached the memory allowance, drop a previously known transaction hash
	for p.knownTxs.Cardinality() >= maxKnownTxs {
		p.knownTxs.Pop()
	}
	p.knownTxs.Add(hash)
}

// Send writes an RLP-encoded message with the given code.
// data should encode as an RLP list.
func (p *peer) Send(msgcode uint64, data interface{}) error {
	return p2p.Send(p.rw, msgcode, data)
}

// SendTransactions sends transactions to the peer and includes the hashes
// in its transaction hash set for future reference.
func (p *peer) SendTransactions(txs types.Transactions) error {
	for _, tx := range txs {
		p.knownTxs.Add(tx.Hash())
	}
	return p2p.Send(p.rw, TxMsg, txs)
}

// AsyncSendTransactions queues list of transactions propagation to a remote
// peer. If the peer's broadcast queue is full, the event is silently dropped.
func (p *peer) AsyncSendTransactions(txs []*types.Transaction) {
	select {
	case p.queuedTxs <- txs:
		for _, tx := range txs {
			p.knownTxs.Add(tx.Hash())
		}
	default:
		//p.Log().Debug("Dropping transaction propagation", "count", len(txs))
	}
}

// sendPooledTransactionHashes sends transaction hashes to the peer and includes
// them in its transaction hash set for future reference.
//
// This method is a helper used by the async transaction announcer. Don't call it
// directly as the queueing (memory) and transmission (bandwidth) costs should
// not be managed directly.
func (p *peer) sendPooledTransactionHashes(hashes []common.Hash) error {

	for _, hash := range hashes {
		p.knownTxs.Add(hash)
	}
	return p2p.Send(p.rw, TxHashesMsg, hashes)
}

// AsyncSendPooledTransactionHashes queues a list of transactions hashes to eventually
// announce to a remote peer.  The number of pending sends are capped (new ones
// will force old sends to be dropped)
func (p *peer) AsyncSendPooledTransactionHashes(hashes []common.Hash) {
	select {
	case p.queuedHashes <- hashes:
		for _, hash := range hashes {
			p.knownTxs.Add(hash)
		}
	case <-p.term:
		p.Log().Debug("Dropping transaction hashes", "count", len(hashes))
	}
}

// SendNewBlockHashes announces the availability of a number of blocks through
// a hash notification.
func (p *peer) SendNewBlockHashes(hashes []common.Hash, numbers []uint64) error {
	for _, hash := range hashes {
		p.knownBlocks.Add(hash)
	}
	request := make(newBlockHashesData, len(hashes))
	for i := 0; i < len(hashes); i++ {
		request[i].Hash = hashes[i]
		request[i].Number = numbers[i]
	}
	return p2p.Send(p.rw, NewBlockHashesMsg, request)
}

// AsyncSendNewBlockHash queues the availability of a block for propagation to a
// remote peer. If the peer's broadcast queue is full, the event is silently
// dropped.
func (p *peer) AsyncSendNewBlockHash(block *types.Block) {
	select {
	case p.queuedAnns <- block:
		p.knownBlocks.Add(block.Hash())
	default:
		p.Log().Debug("Dropping block announcement", "number", block.NumberU64(), "hash", block.Hash())
	}
}

// SendNewBlock propagates an entire block to a remote peer.
func (p *peer) SendNewBlock(block *types.Block) error {
	p.knownBlocks.Add(block.Hash())
	return p2p.Send(p.rw, NewBlockMsg, []interface{}{block})
}

// AsyncSendNewBlock queues an entire block for propagation to a remote peer. If
// the peer's broadcast queue is full, the event is silently dropped.
func (p *peer) AsyncSendNewBlock(block *types.Block) {
	select {
	case p.queuedProps <- &propEvent{block: block}:
		p.knownBlocks.Add(block.Hash())
	default:
		p.Log().Debug("Dropping block propagation", "number", block.NumberU64(), "hash", block.Hash())
	}
}

// SendBlockHeaders sends a batch of block headers to the remote peer.
func (p *peer) SendBlockHeaders(headers []*types.Header) error {
	return p2p.Send(p.rw, BlockHeadersMsg, headers)
}

// SendBlockBodies sends a batch of block contents to the remote peer.
func (p *peer) SendBlockBodies(bodies []*blockBody) error {
	return p2p.Send(p.rw, BlockBodiesMsg, blockBodiesData(bodies))
}

// SendBlockBodiesRLP sends a batch of block contents to the remote peer from
// an already RLP encoded format.
func (p *peer) SendBlockBodiesRLP(bodies []rlp.RawValue) error {
	return p2p.Send(p.rw, BlockBodiesMsg, bodies)
}

// SendNodeDataRLP sends a batch of arbitrary internal data, corresponding to the
// hashes requested.
func (p *peer) SendNodeData(data [][]byte) error {
	return p2p.Send(p.rw, NodeDataMsg, data)
}

// SendReceiptsRLP sends a batch of transaction receipts, corresponding to the
// ones requested from an already RLP encoded format.
func (p *peer) SendReceiptsRLP(receipts []rlp.RawValue) error {
	return p2p.Send(p.rw, ReceiptsMsg, receipts)
}

// SendPooledTransactionsRLP sends requested transactions to the peer and adds the
// hashes in its transaction hash set for future reference.
//
// Note, the method assumes the hashes are correct and correspond to the list of
// transactions being sent.
func (p *peer) SendPooledTransactionsRLP(hashes []common.Hash, txs []rlp.RawValue) error {
	// Mark all the transactions as known, but ensure we don't overflow our limits
	for p.knownTxs.Cardinality() > max(0, maxKnownTxs-len(hashes)) {
		p.knownTxs.Pop()
	}
	for _, hash := range hashes {
		p.knownTxs.Add(hash)
	}
	return p2p.Send(p.rw, PooledTxMsg, txs)
}

// RequestOneHeader is a wrapper around the header query functions to fetch a
// single header. It is used solely by the fetcher.
func (p *peer) RequestOneHeader(hash common.Hash) error {
	p.Log().Debug("Fetching single header", "hash", hash)
	return p2p.Send(p.rw, GetBlockHeadersMsg, &getBlockHeadersData{Origin: hashOrNumber{Hash: hash}, Amount: uint64(1), Skip: uint64(0), Reverse: false})
}

// RequestHeadersByHash fetches a batch of blocks' headers corresponding to the
// specified header query, based on the hash of an origin block.
func (p *peer) RequestHeadersByHash(origin common.Hash, amount int, skip int, reverse bool) error {
	p.Log().Debug("Fetching batch of headers", "count", amount, "fromhash", origin, "skip", skip, "reverse", reverse)
	return p2p.Send(p.rw, GetBlockHeadersMsg, &getBlockHeadersData{Origin: hashOrNumber{Hash: origin}, Amount: uint64(amount), Skip: uint64(skip), Reverse: reverse})
}

// RequestHeadersByNumber fetches a batch of blocks' headers corresponding to the
// specified header query, based on the number of an origin block.
func (p *peer) RequestHeadersByNumber(origin uint64, amount int, skip int, reverse bool) error {
	p.Log().Debug("Fetching batch of headers", "count", amount, "fromnum", origin, "skip", skip, "reverse", reverse)
	return p2p.Send(p.rw, GetBlockHeadersMsg, &getBlockHeadersData{Origin: hashOrNumber{Number: origin}, Amount: uint64(amount), Skip: uint64(skip), Reverse: reverse})
}

// RequestBodies fetches a batch of blocks' bodies corresponding to the hashes
// specified.
func (p *peer) RequestBodies(hashes []common.Hash) error {
	p.Log().Debug("Fetching batch of block bodies", "count", len(hashes))
	return p2p.Send(p.rw, GetBlockBodiesMsg, hashes)
}

// RequestNodeData fetches a batch of arbitrary data from a node's known state
// data, corresponding to the specified hashes.
func (p *peer) RequestNodeData(hashes []common.Hash) error {
	p.Log().Debug("Fetching batch of state data", "count", len(hashes))
	return p2p.Send(p.rw, GetNodeDataMsg, hashes)
}

// RequestReceipts fetches a batch of transaction receipts from a remote node.
func (p *peer) RequestReceipts(hashes []common.Hash) error {
	p.Log().Debug("Fetching batch of receipts", "count", len(hashes))
	return p2p.Send(p.rw, GetReceiptsMsg, hashes)
}

// RequestTxs fetches a batch of transactions from a remote node.
func (p *peer) RequestTxs(hashes []common.Hash) error {
	p.Log().Debug("Fetching batch of transactions", "count", len(hashes))
	return p2p.Send(p.rw, GetPooledTxMsg, hashes)
}

// Handshake executes the eth protocol handshake, negotiating version number,
// network IDs, difficulties, head and genesis blocks.
func (p *peer) Handshake(network uint64, bn *big.Int, head common.Hash, genesis common.Hash) error {
	// Send out own handshake in a new thread
	errc := make(chan error, 2)
	var status statusData // safe to read after two values have been received from errc

	go func() {
		scb, err := json.Marshal(common.SysCfg.ReplayParam.OldSysContracts)
		if err != nil {
			errc <- err
			return
		}
		errc <- p2p.Send(p.rw, StatusMsg, &statusData{
			ProtocolVersion:       uint32(p.version),
			NetworkId:             network,
			BN:                    bn,
			CurrentBlock:          head,
			GenesisBlock:          genesis,
			ReplayPovit:           common.SysCfg.ReplayParam.Pivot,
			ReplayOldSuperAdmin:   common.SysCfg.ReplayParam.OldSuperAdmin,
			ReplayOldSysContracts: scb,
		})
	}()
	go func() {
		errc <- p.readStatus(network, &status, genesis)
	}()
	timeout := time.NewTimer(handshakeTimeout)
	defer timeout.Stop()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errc:
			if err != nil {
				return err
			}
		case <-timeout.C:
			return p2p.DiscReadTimeout
		}
	}
	p.bn, p.head = status.BN, status.CurrentBlock
	p.replayParam.Pivot = status.ReplayPovit
	p.replayParam.OldSuperAdmin = status.ReplayOldSuperAdmin

	m := make(map[common.Address]string)
	if err := json.Unmarshal(status.ReplayOldSysContracts, &m); err != nil {
		return err
	}
	p.replayParam.OldSysContracts = m
	return nil
}

func (p *peer) readStatus(network uint64, status *statusData, genesis common.Hash) (err error) {
	msg, err := p.rw.ReadMsg()
	if err != nil {
		return err
	}
	if msg.Code != StatusMsg {
		return errResp(ErrNoStatusMsg, "first msg has code %x (!= %x)", msg.Code, StatusMsg)
	}
	if msg.Size > ProtocolMaxMsgSize {
		return errResp(ErrMsgTooLarge, "%v > %v", msg.Size, ProtocolMaxMsgSize)
	}
	// Decode the handshake and make sure everything matches
	if err := msg.Decode(&status); err != nil {
		return errResp(ErrDecode, "msg %v: %v", msg, err)
	}
	if status.GenesisBlock != genesis {
		return errResp(ErrGenesisBlockMismatch, "%x (!= %x)", status.GenesisBlock[:8], genesis[:8])
	}
	if status.NetworkId != network {
		return errResp(ErrNetworkIdMismatch, "%d (!= %d)", status.NetworkId, network)
	}
	if int(status.ProtocolVersion) != p.version {
		return errResp(ErrProtocolVersionMismatch, "%d (!= %d)", status.ProtocolVersion, p.version)
	}
	return nil
}

// String implements fmt.Stringer.
func (p *peer) String() string {
	return fmt.Sprintf("Peer %s [%s]", p.id,
		fmt.Sprintf("eth/%2d", p.version),
	)
}

// peerSet represents the collection of active peers currently participating in
// the Ethereum sub-protocol.
type peerSet struct {
	peers  map[string]*peer
	lock   sync.RWMutex
	closed bool
}

// newPeerSet creates a new peer set to track the active participants.
func newPeerSet() *peerSet {
	return &peerSet{
		peers: make(map[string]*peer),
	}
}

// Register injects a new peer into the working set, or returns an error if the
// peer is already known. If a new peer it registered, its broadcast loop is also
// started.
func (ps *peerSet) Register(p *peer, removePeer func(string)) error {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	if ps.closed {
		return errClosed
	}
	if _, ok := ps.peers[p.id]; ok {
		return errAlreadyRegistered
	}
	ps.peers[p.id] = p
	go p.broadcast(removePeer)

	return nil
}

// Unregister removes a remote peer from the active set, disabling any further
// actions to/from that particular entity.
func (ps *peerSet) Unregister(id string) error {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	p, ok := ps.peers[id]
	if !ok {
		return errNotRegistered
	}
	delete(ps.peers, id)
	p.close()

	return nil
}

// Peers returns all registered peers
func (ps *peerSet) Peers() map[string]*peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	set := make(map[string]*peer)
	for id, p := range ps.peers {
		set[id] = p
	}
	return set
}

// Peer retrieves the registered peer with the given id.
func (ps *peerSet) Peer(id string) *peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return ps.peers[id]
}

// Len returns if the current number of peers in the set.
func (ps *peerSet) Len() int {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return len(ps.peers)
}

// PeersWithoutBlock retrieves a list of peers that do not have a given block in
// their set of known hashes.
func (ps *peerSet) PeersWithoutBlock(hash common.Hash) []*peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*peer, 0, len(ps.peers))
	for _, p := range ps.peers {
		if !p.knownBlocks.Contains(hash) {
			list = append(list, p)
		}
	}
	return list
}

// PeersWithoutTx retrieves a list of peers that do not have a given transaction
// in their set of known hashes.
func (ps *peerSet) PeersWithoutTx(hash common.Hash) []*peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*peer, 0, len(ps.peers))
	for _, p := range ps.peers {
		if !p.knownTxs.Contains(hash) {
			list = append(list, p)
		}
	}
	return list
}

func (ps *peerSet) ConsensusPeers() []*peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*peer, 0, len(ps.peers))
	for _, p := range ps.peers {
		if p.IsConsensus() {
			list = append(list, p)
		}
	}
	return list
}

// ConsensusPeersWithoutTx retrieves a list of consensus peers that do not have a given transaction
// in their set of known hashes.
func (ps *peerSet) ConsensusPeersWithoutTx(csPeers []*peer, hash common.Hash) []*peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*peer, 0, len(csPeers))
	for _, p := range csPeers {
		if _, ok := ps.peers[p.id]; ok {
			if !p.knownTxs.Contains(hash) {
				list = append(list, p)
			}
		}
	}
	return list
}

// BestPeer retrieves the known peer with the currently highest total difficulty.
func (ps *peerSet) BestPeer() *peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	var (
		bestPeer *peer
		bestBn   *big.Int
	)
	for _, p := range ps.peers {
		if _, bn := p.Head(); bestPeer == nil || bn.Cmp(bestBn) > 0 {
			bestPeer, bestBn = p, bn
		}
	}
	return bestPeer
}

// Close disconnects all peers.
// No new peers can be registered after Close has returned.
func (ps *peerSet) Close() {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	for _, p := range ps.peers {
		p.Disconnect(p2p.DiscQuitting)
	}
	ps.closed = true
}

func (ps *peerSet) PeersWithConsensus(engine consensus.Engine) []*peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return nil
}

func (ps *peerSet) PeersWithoutConsensus(engine consensus.Engine) []*peer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	consensusNodeMap := make(map[string]string)
	list := make([]*peer, 0, len(ps.peers))
	for nodeId, peer := range ps.peers {
		if _, ok := consensusNodeMap[nodeId]; !ok {
			list = append(list, peer)
		}
	}

	return list
}

type preBlockEvent struct {
	block *types.Block
}

type signatureEvent struct {
	SignHash  common.Hash // Signature hash，header[0:32]
	Hash      common.Hash // Block hash，header[:]
	Number    *big.Int
	Signature *common.BlockConfirmSign
}

// SendPrepareBlock propagates an entire block to a remote peer.
func (p *peer) SendPrepareBlock(block *types.Block) error {
	return p2p.Send(p.rw, PrepareBlockMsg, []interface{}{block})
}

func (p *peer) AsyncSendPrepareBlock(block *types.Block) {
	select {
	case p.queuedPreBlock <- &preBlockEvent{block: block}:
		p.Log().Debug("Send prepare block propagation", "number", block.NumberU64(), "hash", block.Hash())
	default:
		p.Log().Debug("Dropping prepare block propagation", "number", block.NumberU64(), "hash", block.Hash())
	}
}
