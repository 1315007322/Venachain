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

package miner

import (
	"math/big"
	"sync"

	"sync/atomic"
	"time"

	"fmt"

	"github.com/Venachain/Venachain/common"
	"github.com/Venachain/Venachain/consensus"
	"github.com/Venachain/Venachain/core"
	"github.com/Venachain/Venachain/core/state"
	"github.com/Venachain/Venachain/core/types"
	"github.com/Venachain/Venachain/core/vm"
	"github.com/Venachain/Venachain/ethdb"
	"github.com/Venachain/Venachain/event"
	"github.com/Venachain/Venachain/log"
	"github.com/Venachain/Venachain/params"
	"github.com/Venachain/Venachain/rpc"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize = 10

	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096

	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10

	// resubmitAdjustChanSize is the size of resubmitting interval adjustment channel.
	resubmitAdjustChanSize = 10

	// miningLogAtDepth is the number of confirmations before logging successful mining.
	miningLogAtDepth = 7

	// minRecommitInterval is the minimal time interval to recreate the mining block with
	// any newly arrived transactions.
	minRecommitInterval = 1 * time.Second

	// maxRecommitInterval is the maximum time interval to recreate the mining block with
	// any newly arrived transactions.
	maxRecommitInterval = 15 * time.Second

	// intervalAdjustRatio is the impact a single interval adjustment has on sealing work
	// resubmitting interval.
	intervalAdjustRatio = 0.1

	// intervalAdjustBias is applied during the new resubmit interval calculation in favor of
	// increasing upper limit or decreasing lower limit so that the limit can be reachable.
	intervalAdjustBias = 200 * 1000.0 * 1000.0

	// staleThreshold is the maximum depth of the acceptable stale block.
	staleThreshold = 7

	defaultCommitRatio = 0.95
)

// environment is the worker's current environment and holds all of the current state information.
type environment struct {
	signer types.Signer

	state   *state.StateDB // apply state changes here
	tcount  int            // tx count in cycle
	gasPool *core.GasPool  // available gas used to pack transactions

	header   *types.Header
	txs      []*types.Transaction
	receipts []*types.Receipt
}

// task contains all information for consensus engine sealing and result submitting.
type task struct {
	receipts  []*types.Receipt
	state     *state.StateDB
	block     *types.Block
	createdAt time.Time
}

const (
	commitInterruptNone int32 = iota
	commitInterruptNewHead
	commitInterruptResubmit
)

// newWorkReq represents a request for new sealing work submitting with relative interrupt notifier.
type newWorkReq struct {
	interrupt   *int32
	timestamp   int64
	commitBlock *types.Block
}

// intervalAdjust represents a resubmitting interval adjustment.
type intervalAdjust struct {
	ratio float64
	inc   bool
}

type commitWorkEnv struct {
	baseLock            sync.RWMutex
	commitBaseBlock     *types.Block
	commitTime          int64
	highestLock         sync.RWMutex
	highestLogicalBlock *types.Block
}

func (e *commitWorkEnv) getHighestLogicalBlock() *types.Block {
	e.highestLock.RLock()
	defer e.highestLock.RUnlock()
	return e.highestLogicalBlock
}

// worker is the main object which takes care of submitting new work to consensus engine
// and gathering the sealing result.
type worker struct {
	extdb  ethdb.Database
	config *params.ChainConfig
	engine consensus.Engine
	eth    Backend
	chain  *core.BlockChain

	gasFloor uint64
	gasCeil  uint64

	// Subscriptions
	mux          *event.TypeMux
	txsCh        chan core.NewTxsEvent
	txsSub       event.Subscription
	chainHeadCh  chan core.ChainHeadEvent
	chainHeadSub event.Subscription

	// Channels
	newWorkCh             chan *newWorkReq
	taskCh                chan *task
	resultCh              chan *types.Block
	prepareResultCh       chan *types.Block
	highestLogicalBlockCh chan *types.Block
	startCh               chan struct{}
	exitCh                chan struct{}
	resubmitIntervalCh    chan time.Duration
	resubmitAdjustCh      chan *intervalAdjust

	current     *environment       // An environment for current running cycle.
	unconfirmed *unconfirmedBlocks // A set of locally mined blocks pending canonicalness confirmations.

	mu       sync.RWMutex // The lock used to protect the coinbase and extra fields
	coinbase common.Address
	extra    []byte

	pendingMu    sync.RWMutex
	pendingTasks map[common.Hash]*task

	snapshotMu    sync.RWMutex // The lock used to protect the block snapshot and state snapshot
	snapshotBlock *types.Block
	snapshotState *state.StateDB

	// atomic status counters
	running int32 // The indicator whether the consensus engine is running or not.
	newTxs  int32 // New arrival transaction count since last sealing work submitting.

	// External functions
	isLocalBlock func(block *types.Block) bool // Function used to determine whether the specified block is mined by local miner.

	blockChainCache *core.BlockChainCache
	commitWorkEnv   *commitWorkEnv
	recommit        time.Duration
	commitDuration  int64 //in Millisecond

	// Test hooks
	newTaskHook  func(*task)                        // Method to call upon receiving a new sealing task.
	skipSealHook func(*task) bool                   // Method to decide whether skipping the sealing.
	fullTaskHook func()                             // Method to call before pushing the full sealing task.
	resubmitHook func(time.Duration, time.Duration) // Method to call upon updating resubmitting interval.
}

func newWorker(config *params.ChainConfig, engine consensus.Engine, eth Backend, mux *event.TypeMux, recommit time.Duration, gasFloor, gasCeil uint64, isLocalBlock func(*types.Block) bool,
	highestLogicalBlockCh chan *types.Block, blockChainCache *core.BlockChainCache) *worker {

	worker := &worker{
		extdb:                 eth.ExtendedDb(),
		config:                config,
		engine:                engine,
		eth:                   eth,
		mux:                   mux,
		chain:                 eth.BlockChain(),
		gasFloor:              gasFloor,
		gasCeil:               gasCeil,
		isLocalBlock:          isLocalBlock,
		unconfirmed:           newUnconfirmedBlocks(eth.BlockChain(), miningLogAtDepth),
		pendingTasks:          make(map[common.Hash]*task),
		txsCh:                 make(chan core.NewTxsEvent, txChanSize),
		chainHeadCh:           make(chan core.ChainHeadEvent, chainHeadChanSize),
		newWorkCh:             make(chan *newWorkReq),
		taskCh:                make(chan *task),
		resultCh:              make(chan *types.Block, resultQueueSize),
		prepareResultCh:       make(chan *types.Block, resultQueueSize),
		exitCh:                make(chan struct{}),
		startCh:               make(chan struct{}, 1),
		resubmitIntervalCh:    make(chan time.Duration),
		resubmitAdjustCh:      make(chan *intervalAdjust, resubmitAdjustChanSize),
		highestLogicalBlockCh: highestLogicalBlockCh,
		blockChainCache:       blockChainCache,
		commitWorkEnv:         &commitWorkEnv{},
	}
	// Subscribe events for blockchain
	worker.chainHeadSub = eth.BlockChain().SubscribeChainHeadEvent(worker.chainHeadCh)

	// Sanitize recommit interval if the user-specified one is too short.
	if recommit < minRecommitInterval {
		log.Warn("Sanitizing miner recommit interval", "provided", recommit, "updated", minRecommitInterval)
		recommit = minRecommitInterval
	}

	worker.recommit = recommit
	worker.commitDuration = int64((float64)(recommit.Nanoseconds()/1e6) * defaultCommitRatio)
	log.Info("commitDuration in Millisecond", "commitDuration", worker.commitDuration)

	go worker.mainLoop()
	go worker.newWorkLoop(recommit)
	go worker.resultLoop()
	go worker.taskLoop()

	// Submit first work to initialize pending state.
	worker.startCh <- struct{}{}

	return worker
}

// setEtherbase sets the etherbase used to initialize the block coinbase field.
func (w *worker) setEtherbase(addr common.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.coinbase = addr
}

// setExtra sets the content used to initialize the block extra field.
func (w *worker) setExtra(extra []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.extra = extra
}

// setRecommitInterval updates the interval for miner sealing work recommitting.
func (w *worker) setRecommitInterval(interval time.Duration) {
	w.resubmitIntervalCh <- interval
}

// pending returns the pending state and corresponding block.
func (w *worker) pending() (*types.Block, *state.StateDB) {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	if w.snapshotState == nil {
		return nil, nil
	}
	return w.snapshotBlock, w.snapshotState.Copy()
}

// pendingBlock returns pending block.
func (w *worker) pendingBlock() *types.Block {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	return w.snapshotBlock
}

// start sets the running status as 1 and triggers new work submitting.
func (w *worker) start() {

	atomic.StoreInt32(&w.running, 1)
	w.startCh <- struct{}{}
	if eng, ok := w.engine.(consensus.Istanbul); ok {
		eng.Start(w.chain, w.chain.CurrentBlock)
	}
}

// stop sets the running status as 0.
func (w *worker) stop() {
	atomic.StoreInt32(&w.running, 0)

	if eng, ok := w.engine.(consensus.Istanbul); ok {
		eng.Stop()
	}
}

// isRunning returns an indicator whether worker is running or not.
func (w *worker) isRunning() bool {
	return atomic.LoadInt32(&w.running) == 1
}

// close terminates all background threads maintained by the worker.
// Note the worker does not support being closed multiple times.
func (w *worker) close() {
	close(w.exitCh)
}

// newWorkLoop is a standalone goroutine to submit new mining work upon received events.
func (w *worker) newWorkLoop(recommit time.Duration) {
	var (
		interrupt   *int32
		minRecommit = recommit // minimal resubmit interval specified by user.
		timestamp   int64      // timestamp for each round of mining in Millisecond.
	)

	timer := time.NewTimer(0)
	<-timer.C // discard the initial tick

	// commit aborts in-flight transaction execution with given signal and resubmits a new one.
	commit := func(s int32, baseBlock *types.Block) {
		if interrupt != nil {
			atomic.StoreInt32(interrupt, s)
		}
		interrupt = new(int32)
		w.newWorkCh <- &newWorkReq{interrupt: interrupt, timestamp: timestamp, commitBlock: baseBlock}
		timer.Reset(recommit)
		atomic.StoreInt32(&w.newTxs, 0)
	}
	// recalcRecommit recalculates the resubmitting interval upon feedback.
	recalcRecommit := func(target float64, inc bool) {
		var (
			prev = float64(recommit.Nanoseconds())
			next float64
		)
		if inc {
			next = prev*(1-intervalAdjustRatio) + intervalAdjustRatio*(target+intervalAdjustBias)
			// Recap if interval is larger than the maximum time interval
			if next > float64(maxRecommitInterval.Nanoseconds()) {
				next = float64(maxRecommitInterval.Nanoseconds())
			}
		} else {
			next = prev*(1-intervalAdjustRatio) + intervalAdjustRatio*(target-intervalAdjustBias)
			// Recap if interval is less than the user specified minimum
			if next < float64(minRecommit.Nanoseconds()) {
				next = float64(minRecommit.Nanoseconds())
			}
		}
		recommit = time.Duration(int64(next))
	}
	// clearPending cleans the stale pending tasks.
	clearPending := func(number uint64) {
		w.pendingMu.Lock()
		for h, t := range w.pendingTasks {
			if t.block.NumberU64()+staleThreshold <= number {
				delete(w.pendingTasks, h)
			}
		}
		w.pendingMu.Unlock()
	}

	for {
		select {
		case <-w.startCh:
			clearPending(w.chain.CurrentBlock().NumberU64())
			timestamp = time.Now().UnixNano() / 1e6
			commit(commitInterruptNewHead, nil)

		case head := <-w.chainHeadCh:
			clearPending(head.Block.NumberU64())
			timestamp = time.Now().UnixNano() / 1e6
			//commit(false, commitInterruptNewHead)
			// clear consensus cache
			log.Info("received a event of ChainHeadEvent", "hash", head.Block.Hash(), "number", head.Block.NumberU64(), "parentHash", head.Block.ParentHash())
			w.blockChainCache.ClearCache(head.Block)

			if h, ok := w.engine.(consensus.Handler); ok {
				h.NewChainHead()
			}
		case <-timer.C:
			// If mining is running resubmit a new work cycle periodically to pull in
			// higher priced transactions. Disable this overhead for pending blocks.
			if !w.isRunning() {
				continue
			}

			if eng, ok := w.engine.(consensus.Istanbul); ok {
				if eng.ShouldSeal() {
					log.Debug("ShouldSeal() -> true")
					commit(commitInterruptResubmit, nil)
					timer.Reset(500 * time.Millisecond)
				} else {
					timer.Reset(50 * time.Millisecond)
				}
			}

		case interval := <-w.resubmitIntervalCh:
			// Adjust resubmit interval explicitly by user.
			if interval < minRecommitInterval {
				log.Warn("Sanitizing miner recommit interval", "provided", interval, "updated", minRecommitInterval)
				interval = minRecommitInterval
			}
			log.Info("Miner recommit interval update", "from", minRecommit, "to", interval)
			minRecommit, recommit = interval, interval

			if w.resubmitHook != nil {
				w.resubmitHook(minRecommit, recommit)
			}
		case adjust := <-w.resubmitAdjustCh:
			// Adjust resubmit interval by feedback.
			if adjust.inc {
				before := recommit
				recalcRecommit(float64(recommit.Nanoseconds())/adjust.ratio, true)
				log.Trace("Increase miner recommit interval", "from", before, "to", recommit)
			} else {
				before := recommit
				recalcRecommit(float64(minRecommit.Nanoseconds()), false)
				log.Trace("Decrease miner recommit interval", "from", before, "to", recommit)
			}

			if w.resubmitHook != nil {
				w.resubmitHook(minRecommit, recommit)
			}
		case <-w.exitCh:
			return
		}
	}
}

// mainLoop is a standalone goroutine to regenerate the sealing task based on the received event.
func (w *worker) mainLoop() {
	// defer w.txsSub.Unsubscribe()
	defer w.chainHeadSub.Unsubscribe()
	//defer w.chainSideSub.Unsubscribe()

	for {
		select {
		case req := <-w.newWorkCh:
			w.commitNewWork(req.interrupt, req.timestamp, req.commitBlock)
		// System stopped
		case <-w.exitCh:
			return
		case <-w.chainHeadSub.Err():
			return

		case block := <-w.prepareResultCh:
			// Short circuit when receiving empty result.
			if block == nil {
				continue
			}
			// Short circuit when receiving duplicate result caused by resubmitting.
			if w.chain.HasBlock(block.Hash(), block.NumberU64()) {
				continue
			}
			var (
				sealhash = w.engine.SealHash(block.Header())
				hash     = block.Hash()
			)
			w.pendingMu.RLock()
			_, exist := w.pendingTasks[sealhash]
			w.pendingMu.RUnlock()
			if !exist {
				log.Error("Block found but no relative pending task", "number", block.Number(), "sealhash", sealhash, "hash", hash)
				continue
			}
		}
	}
}

// taskLoop is a standalone goroutine to fetch sealing task from the generator and
// push them to consensus engine.
func (w *worker) taskLoop() {
	var (
		stopCh chan struct{}
		prev   common.Hash
	)

	// interrupt aborts the in-flight sealing task.
	interrupt := func() {
		if stopCh != nil {
			close(stopCh)
			stopCh = nil
		}
	}
	for {
		select {
		case task := <-w.taskCh:
			if w.newTaskHook != nil {
				w.newTaskHook(task)
			}
			// Reject duplicate sealing work due to resubmitting.
			sealHash := w.engine.SealHash(task.block.Header())
			if sealHash == prev {
				continue
			}
			// Interrupt previous sealing operation
			interrupt()
			stopCh, prev = make(chan struct{}), sealHash

			if w.skipSealHook != nil && w.skipSealHook(task) {
				continue
			}

			isEmpty := task.block.Transactions().Len() == 0
			isProduceEmptyBlock := common.SysCfg.IsProduceEmptyBlock()

			if !isEmpty || isProduceEmptyBlock {
				w.pendingMu.Lock()
				w.pendingTasks[sealHash] = task
				w.pendingMu.Unlock()
			}

			if _, ok := w.engine.(consensus.Istanbul); ok {
				// todo: shouldSeal()
				if _, err := w.engine.Seal(w.chain, task.block, w.resultCh, stopCh); err != nil {
					log.Warn("Block sealing failed", "err", err)
				}
				continue
			}

			if _, err := w.engine.Seal(w.chain, task.block, w.resultCh, stopCh); err != nil {
				log.Warn("Block sealing failed", "err", err)
			}

		case <-w.exitCh:
			interrupt()
			return
		}
	}
}

// resultLoop is a standalone goroutine to handle sealing result submitting
// and flush relative data to the database.
func (w *worker) resultLoop() {
	for {
		select {
		case block := <-w.resultCh:
			now := time.Now()
			// Short circuit when receiving empty result.
			if block == nil {
				continue
			}
			// Short circuit when receiving duplicate result caused by resubmitting.
			if w.chain.HasBlock(block.Hash(), block.NumberU64()) {
				continue
			}
			var (
				sealhash = w.engine.SealHash(block.Header())
				hash     = block.Hash()
			)
			w.pendingMu.RLock()
			task, exist := w.pendingTasks[sealhash]
			w.pendingMu.RUnlock()
			if !exist {
				log.Error("Block found but no relative pending task", "number", block.Number(), "sealhash", sealhash, "hash", hash)
				continue
			}

			// Different block could share same sealhash, deep copy here to prevent write-write conflict.
			var (
				//receipts = make([]*types.Receipt, len(task.receipts))
				logs []*types.Log
			)
			for _, receipt := range task.receipts {
				//receipts[i] = new(types.Receipt)
				//*receipts[i] = *receipt
				// Update the block hash in all logs since it is now available and not when the
				// receipt/log of individual transactions were created.
				for _, log := range receipt.Logs {
					log.BlockHash = hash
				}
				logs = append(logs, receipt.Logs...)
			}
			// Commit block and state to database.
			stat, err := w.chain.WriteBlockWithState(block, task.receipts, task.state, false)
			if err != nil {
				log.Error("Failed writing block to chain", "err", err)
				continue
			}
			log.Info("Successfully sealed new block", "number", block.Number(), "sealhash", sealhash, "hash", hash,
				"elapsed", common.PrettyDuration(time.Since(task.createdAt)))
			// Broadcast the block and announce chain insertion event
			w.mux.Post(core.NewMinedBlockEvent{Block: block})

			var events []interface{}
			switch stat {
			case core.CanonStatTy:
				log.Debug("Prepare Events, WriteStatus=CanonStatTy")
				events = append(events, core.ChainEvent{Block: block, Hash: block.Hash(), Logs: logs})
				events = append(events, core.ChainHeadEvent{Block: block})
			case core.SideStatTy:
				log.Debug("Prepare Events, WriteStatus=SideStatTy")
				events = append(events, core.ChainSideEvent{Block: block})
			}
			w.chain.PostChainEvents(events, logs)

			// Insert the block into the set of pending ones to resultLoop for confirmations
			//w.unconfirmed.Insert(block.NumberU64(), block.Hash())

			log.Info("result block ---------------------------", "duration", time.Since(now))
		case <-w.exitCh:
			return
		}
	}
}

// makeCurrent creates a new environment for the current cycle.
func (w *worker) makeCurrent(parent *types.Block, header *types.Header) error {
	var (
		state *state.StateDB
		err   error
	)

	state, err = w.chain.StateAt(parent.Root())

	if err != nil {
		return err
	}

	env := &environment{
		signer: types.NewEIP155Signer(w.config.ChainID),
		state:  state,
		header: header,
	}

	// Keep track of transactions which return errors so they can be removed
	env.tcount = 0
	w.current = env
	return nil
}

// updateSnapshot updates pending snapshot block and state.
// Note this function assumes the current variable is thread safe.
func (w *worker) updateSnapshot(block *types.Block) {
	w.snapshotMu.Lock()
	defer w.snapshotMu.Unlock()
	if block == nil {
		w.snapshotBlock = types.NewBlock(
			w.current.header,
			w.current.txs,
			w.current.receipts,
		)
	} else {
		w.snapshotBlock = block
	}
	w.snapshotState = w.current.state.Copy()
}

func (w *worker) commitTransaction(tx *types.Transaction, coinbase common.Address) ([]*types.Log, error) {
	snap := w.current.state.Snapshot()

	receipt, _, err := core.ApplyTransaction(w.config, w.chain, &coinbase, w.current.gasPool, w.current.state, w.current.header, tx, &w.current.header.GasUsed, vm.Config{})
	if err != nil {
		w.current.state.RevertToSnapshot(snap)
		return nil, err
	}
	w.current.txs = append(w.current.txs, tx)
	w.current.receipts = append(w.current.receipts, receipt)

	return receipt.Logs, nil
}

func (w *worker) commitTransactionsWithHeader(header *types.Header, txs *types.TransactionsByPriceAndNonce, coinbase common.Address, interrupt *int32) bool {
	// Short circuit if current is nil
	//timeout := false

	if w.current == nil {
		return true
	}

	if w.current.gasPool == nil {
		w.current.gasPool = new(core.GasPool).AddGas(w.current.header.GasLimit)
	}

	var coalescedLogs []*types.Log

	for {
		// In the following three cases, we will interrupt the execution of the transaction.
		// (1) new head block event arrival, the interrupt signal is 1
		// (2) worker start or restart, the interrupt signal is 1
		// (3) worker recreate the mining block with any newly arrived transactions, the interrupt signal is 2.
		// For the first two cases, the semi-finished work will be discarded.
		// For the third case, the semi-finished work will be submitted to the consensus engine.
		if interrupt != nil && atomic.LoadInt32(interrupt) != commitInterruptNone {
			// Notify resubmit loop to increase resubmitting interval due to too frequent commits.
			if atomic.LoadInt32(interrupt) == commitInterruptResubmit {
				ratio := float64(w.current.header.GasLimit-w.current.gasPool.Gas()) / float64(w.current.header.GasLimit)
				if ratio < 0.1 {
					ratio = 0.1
				}
				w.resubmitAdjustCh <- &intervalAdjust{
					ratio: ratio,
					inc:   true,
				}
			}
			return atomic.LoadInt32(interrupt) == commitInterruptNewHead
		}
		// If we don't have enough gas for any further transactions then we're done
		if w.current.gasPool.Gas() < params.TxGas {
			log.Trace("Not enough gas for further transactions", "have", w.current.gasPool, "want", params.TxGas)
			break
		}
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		if tx == nil {
			break
		}
		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		from, _ := types.Sender(w.current.signer, tx)

		// Start executing the transaction
		rpc.MonitorWriteData(rpc.TransactionExecuteStartTime, tx.Hash().String(), "", w.extdb)
		w.current.state.Prepare(tx.Hash(), common.Hash{}, w.current.tcount)
		txHash := tx.Hash()
		log.Trace("Start executing the transaction", "txHash", fmt.Sprintf("%x", txHash[:log.LogHashLen]), "blockNumber", header.Number)
		logs, err := w.commitTransaction(tx, coinbase)
		rpc.MonitorWriteData(rpc.TransactionExecuteEndTime, tx.Hash().String(), "", w.extdb)
		switch err {
		case core.ErrGasLimitReached:
			// Pop the current out-of-gas transaction without shifting in the next from the account
			log.Warn("Gas limit exceeded for current block", "blockNumber", header.Number, "blockParentHash", header.ParentHash, "tx.hash", tx.Hash(), "sender", from, "senderCurNonce", w.current.state.GetNonce(from), "tx.nonce", tx.Nonce())
			txs.Pop()
			rpc.MonitorWriteData(rpc.TransactionExecuteStatus, tx.Hash().String(), "false", w.extdb)
		case core.ErrNonceTooLow:
			// New head notification data race between the transaction pool and miner, shift
			log.Warn("Skipping transaction with low nonce", "blockNumber", header.Number, "blockParentHash", header.ParentHash, "tx.hash", tx.Hash(), "sender", from, "senderCurNonce", w.current.state.GetNonce(from), "tx.nonce", tx.Nonce())
			txs.Shift()
			rpc.MonitorWriteData(rpc.TransactionExecuteStatus, tx.Hash().String(), "false", w.extdb)
		case core.ErrNonceTooHigh:
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Warn("Skipping account with hight nonce", "blockNumber", header.Number, "blockParentHash", header.ParentHash, "tx.hash", tx.Hash(), "sender", from, "senderCurNonce", w.current.state.GetNonce(from), "tx.nonce", tx.Nonce())
			txs.Pop()
			rpc.MonitorWriteData(rpc.TransactionExecuteStatus, tx.Hash().String(), "false", w.extdb)
		case nil:
			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			w.current.tcount++
			txs.Shift()
			rpc.MonitorWriteData(rpc.TransactionExecuteStatus, tx.Hash().String(), "true", w.extdb)
		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Warn("Transaction failed, account skipped", "blockNumber", header.Number, "blockParentHash", header.ParentHash, "hash", tx.Hash(), "hash", tx.Hash(), "err", err)
			txs.Shift()
			rpc.MonitorWriteData(rpc.TransactionExecuteStatus, tx.Hash().String(), "false", w.extdb)
		}
	}

	if !w.isRunning() && len(coalescedLogs) > 0 {
		// We don't push the pendingLogsEvent while we are mining. The reason is that
		// when we are mining, the worker will regenerate a mining block every 3 seconds.
		// In order to avoid pushing the repeated pendingLog, we disable the pending log pushing.

		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		go w.mux.Post(core.PendingLogsEvent{Logs: cpy})
	}
	// Notify resubmit loop to decrease resubmitting interval if current interval is larger
	// than the user-specified one.
	if interrupt != nil {
		w.resubmitAdjustCh <- &intervalAdjust{inc: false}
	}
	return false
}

// commitNewWork generates several new sealing tasks based on the parent block.
func (w *worker) commitNewWork(interrupt *int32, timestamp int64, commitBlock *types.Block) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	tstart := time.Now()

	var parent *types.Block
	if _, ok := w.engine.(consensus.Istanbul); ok {
		parent = w.chain.CurrentBlock()
		//log.Info("parentBlock Number: " + parent.Number().String())
	} else {
		parent = w.chain.CurrentBlock()
		if parent.Time().Cmp(new(big.Int).SetInt64(timestamp)) >= 0 {
			timestamp = parent.Time().Int64() + 1
		}
		// this will ensure we're not going off too far in the future
		if now := time.Now().Unix(); timestamp > now+1 {
			wait := time.Duration(timestamp-now) * time.Second
			log.Info("Mining too far in the future", "wait", common.PrettyDuration(wait))
			time.Sleep(wait)
		}
	}

	num := parent.Number()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   core.CalcGasLimit(parent, w.gasFloor, w.gasCeil),
		Extra:      w.extra,
		Time:       big.NewInt(timestamp),
	}
	// Only set the coinbase if our consensus engine is running (avoid spurious block rewards)
	if w.isRunning() {
		/*
			if w.coinbase == (common.Address{}) {
				log.Error("Refusing to mine without etherbase")
				return
			}
		*/
		header.Coinbase = w.coinbase
	}

	log.Debug("Begin consensus for new block", "number", header.Number, "gasLimit", header.GasLimit, "parentHash", parent.Hash(), "parentNumber", parent.NumberU64(), "parentStateRoot", parent.Root(), "timestamp", time.Now().UnixNano()/1e6)
	if err := w.engine.Prepare(w.chain, header); err != nil {
		log.Debug("Failed to prepare header for mining", "err", err)
		return
	}

	header.Coinbase = w.coinbase

	// Could potentially happen if starting to mine in an odd state.
	err := w.makeCurrent(parent, header)
	if err != nil {
		log.Error("Failed to create mining context", "err", err)
		return
	}

	// Fill the block with all available pending transactions.
	startTime := time.Now()
	pending, err := w.eth.TxPool().PendingLimited()

	if err != nil {
		log.Error("Failed to fetch pending transactions", "time", common.PrettyDuration(time.Since(startTime)), "err", err)
		return
	}

	//log.Info("Fetch pending transactions success", "pendingLength", len(pending), "time", common.PrettyDuration(time.Since(startTime)))

	// Short circuit if there is no available pending transactions
	if len(pending) == 0 {
		if _, ok := w.engine.(consensus.Istanbul); ok {
			w.commit(nil, true, tstart)
		} else {
			w.updateSnapshot(nil)
		}
		return
	}

	txsCount := 0
	for _, accTxs := range pending {
		txsCount = txsCount + len(accTxs)
	}
	// Split the pending transactions into locals and remotes
	localTxs, remoteTxs := make(map[common.Address]types.Transactions), pending
	for _, account := range w.eth.TxPool().Locals() {
		if txs := remoteTxs[account]; len(txs) > 0 {
			delete(remoteTxs, account)
			localTxs[account] = txs
		}
	}
	log.Debug("execute pending transactions", "localTxCount", len(localTxs), "remoteTxCount", len(remoteTxs), "txsCount", txsCount)

	startTime = time.Now()
	if len(localTxs) > 0 {
		txs := types.NewTransactionsByPriceAndNonce(w.current.signer, localTxs)
		if ok := w.commitTransactionsWithHeader(header, txs, w.coinbase, interrupt); ok {
			return
		}
	}
	if len(remoteTxs) > 0 {
		txs := types.NewTransactionsByPriceAndNonce(w.current.signer, remoteTxs)
		if ok := w.commitTransactionsWithHeader(header, txs, w.coinbase, interrupt); ok {
			return
		}
	}
	log.Info("commit transaction -------------------", "duration", time.Since(startTime))

	w.commit(w.fullTaskHook, true, tstart)
}

// commit runs any post-transaction state modifications, assembles the final block
// and commits new work if consensus engine is running.
func (w *worker) commit(interval func(), update bool, start time.Time) error {
	// Deep copy receipts here to avoid interaction between different tasks.
	receipts := make([]*types.Receipt, len(w.current.receipts))
	for i, l := range w.current.receipts {
		receipts[i] = new(types.Receipt)
		*receipts[i] = *l
	}
	s := w.current.state
	now := time.Now()
	block, err := w.engine.Finalize(w.chain, w.current.header, s, w.current.txs, w.current.receipts)
	log.Info("engine Finalize block ---------------", "duration", time.Since(now))
	if err != nil {
		return err
	}
	if w.isRunning() {
		if interval != nil {
			interval()
		}
		select {
		case w.taskCh <- &task{receipts: receipts, state: s, block: block, createdAt: time.Now()}:
			//w.unconfirmed.Shift(block.NumberU64() - 1)

			feesWei := new(big.Int)
			for i, tx := range block.Transactions() {
				feesWei.Add(feesWei, new(big.Int).Mul(new(big.Int).SetUint64(receipts[i].GasUsed), tx.GasPrice()))
			}
			feesEth := new(big.Float).Quo(new(big.Float).SetInt(feesWei), new(big.Float).SetInt(big.NewInt(params.Ether)))

			log.Info("Commit new mining work", "number", block.Number(), "sealhash", w.engine.SealHash(block.Header()), "receiptHash", block.ReceiptHash(),
				"txs", w.current.tcount, "gas", block.GasUsed(), "fees", feesEth, "elapsed", common.PrettyDuration(time.Since(start)))

		case <-w.exitCh:
			log.Info("Worker has exited")
		}
	}
	if update {
		w.updateSnapshot(block)
	}
	return nil
}

func (w *worker) makePending() (*types.Block, *state.StateDB) {
	var parent = w.commitWorkEnv.getHighestLogicalBlock()
	var parentChain = w.chain.CurrentBlock()

	if parentChain.NumberU64() >= parent.NumberU64() {
		parent = parentChain
	}
	log.Debug("parent in makePending", "number", parent.NumberU64(), "hash", parent.Hash())

	if parent != nil {
		state, err := w.blockChainCache.MakeStateDB(parent)
		if err == nil {
			block := types.NewBlock(
				parent.Header(),
				parent.Transactions(),
				nil,
			)

			return block, state
		}
	}
	return nil, nil
}

//
//func (w *worker) shouldCommit(timestamp int64) (bool, *types.Block) {
//	w.commitWorkEnv.baseLock.Lock()
//	defer w.commitWorkEnv.baseLock.Unlock()
//
//	baseBlock, commitTime := w.commitWorkEnv.commitBaseBlock, w.commitWorkEnv.commitTime
//	highestLogicalBlock := w.commitWorkEnv.getHighestLogicalBlock()
//
//	shouldCommit := false
//	if baseBlock == nil || baseBlock.Hash().Hex() != highestLogicalBlock.Hash().Hex() {
//		shouldCommit = true
//	} else {
//		pending, err := w.eth.TxPool().PendingLimited()
//		if err == nil && len(pending) > 0 {
//			log.Info("w.eth.TxPool()", "pending:", len(pending))
//			shouldCommit = true
//		}
//	}
//
//	if shouldCommit && timestamp != 0 {
//		shouldCommit = (timestamp - commitTime) >= w.recommit.Nanoseconds()/1e6
//	}
//	if shouldCommit {
//		w.commitWorkEnv.commitBaseBlock = highestLogicalBlock
//		w.commitWorkEnv.commitTime = time.Now().UnixNano() / 1e6
//
//		if baseBlock != nil {
//			log.Info("baseBlock", "number", baseBlock.NumberU64(), "hash", baseBlock.Hash(), "hashHex", baseBlock.Hash().Hex())
//			log.Info("commitTime", "commitTime", commitTime, "timestamp", timestamp)
//			log.Info("highestLogicalBlock", "number", highestLogicalBlock.NumberU64(), "hash", highestLogicalBlock.Hash(), "hashHex", highestLogicalBlock.Hash().Hex())
//		}
//	}
//	return shouldCommit, highestLogicalBlock
//}

func (w *worker) resetDone() bool {
	if w.chain.CurrentBlock().Number().Cmp(w.eth.TxPool().GetResetNumber()) == 0 {
		return true
	}
	return false
}
