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
	"bytes"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"gopkg.in/fatih/set.v0"
)

const (
	resultQueueSize  = 10
	miningLogAtDepth = 5

	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096
	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10
	// chainSideChanSize is the size of channel listening to ChainSideEvent.
	chainSideChanSize = 10
)

// Agent can register themself with the worker
type Agent interface {
	Work() chan<- *Work
	SetReturnCh(chan<- *Result)
	Stop()
	Start()
	GetHashRate() int64
}

// Work is the workers current environment and holds
// all of the current state information
type Work struct {
	config *params.ChainConfig
	signer types.Signer

	//状态树管理器
	state     *state.StateDB // apply state changes here
	//祖先区块列表,用于判断叔父的合法性
	//待打包区块的前7个区块
	ancestors *set.Set       // ancestor set (used for checking uncle parent validity)
	//家庭成员列表，用于判断叔块的合法性
	//待打包区块的前7个区块以及其包含的叔块
	family    *set.Set       // family set (used for checking uncle invalidity)
	//叔块列表
	uncles    *set.Set       // uncle set
	//当前挖矿周期的内的交易计数
	tcount    int            // tx count in cycle
	//一个区块的gaslimit， 在应用交易的过程中会递减， 如果剩余的gasPool的量已经不足以支持下一个交易，则停止打包交易
	gasPool   *core.GasPool  // available gas used to pack transactions

	//当前等待挖的区块
	Block *types.Block // the new block

	//区块头
	header   *types.Header
	txs      []*types.Transaction
	receipts []*types.Receipt

	//work创建时间
	createdAt time.Time
}

type Result struct {
	Work  *Work
	Block *types.Block
}

// worker is the main object which takes care of applying messages to the new state
type worker struct {
	config *params.ChainConfig
	engine consensus.Engine

	mu sync.Mutex

	// update loop
	mux          *event.TypeMux
	txsCh        chan core.NewTxsEvent
	txsSub       event.Subscription
	chainHeadCh  chan core.ChainHeadEvent
	chainHeadSub event.Subscription
	chainSideCh  chan core.ChainSideEvent
	chainSideSub event.Subscription
	wg           sync.WaitGroup

	agents map[Agent]struct{}
	recv   chan *Result

	eth     Backend
	chain   *core.BlockChain
	proc    core.Validator
	chainDb ethdb.Database

	coinbase common.Address
	extra    []byte

	currentMu sync.Mutex
	current   *Work

	//
	snapshotMu    sync.RWMutex
	snapshotBlock *types.Block
	snapshotState *state.StateDB

	uncleMu        sync.Mutex
	possibleUncles map[common.Hash]*types.Block

	unconfirmed *unconfirmedBlocks // set of locally mined blocks pending canonicalness confirmations

	// atomic status counters
	mining int32
	atWork int32 //
}

//主要功能:初始化work类,向agent提交一个新工作
//task1:使用外部传入的参数或者默认参数初始化worker类
//task2:注册txpool更新和BlockChain更新事件监听管道
//task3:启动事件监听go程
//task4:等待agent挖好矿的区块
//task5:向agent提交一个新挖矿的工作
func newWorker(config *params.ChainConfig, engine consensus.Engine, coinbase common.Address, eth Backend, mux *event.TypeMux) *worker {
	//--------------------------------------task1--------------------------------------
	//task1:使用外部传入的参数或者默认参数初始化worker类
	//---------------------------------------------------------------------------------
	worker := &worker{
		config:         config,
		engine:         engine,
		eth:            eth,
		mux:            mux,
		txsCh:          make(chan core.NewTxsEvent, txChanSize),
		chainHeadCh:    make(chan core.ChainHeadEvent, chainHeadChanSize),
		chainSideCh:    make(chan core.ChainSideEvent, chainSideChanSize),
		chainDb:        eth.ChainDb(),
		recv:           make(chan *Result, resultQueueSize),
		chain:          eth.BlockChain(),
		proc:           eth.BlockChain().Validator(),
		possibleUncles: make(map[common.Hash]*types.Block),
		coinbase:       coinbase,
		agents:         make(map[Agent]struct{}),
		unconfirmed:    newUnconfirmedBlocks(eth.BlockChain(), miningLogAtDepth),
	}
	//--------------------------------------task2--------------------------------------
	//task2:注册txpool更新和BlockChain更新事件监听管道
	//---------------------------------------------------------------------------------
	// Subscribe NewTxsEvent for tx pool
	worker.txsSub = eth.TxPool().SubscribeNewTxsEvent(worker.txsCh)
	// Subscribe events for blockchain
	worker.chainHeadSub = eth.BlockChain().SubscribeChainHeadEvent(worker.chainHeadCh)
	worker.chainSideSub = eth.BlockChain().SubscribeChainSideEvent(worker.chainSideCh)

	//--------------------------------------task3--------------------------------------
	//task3:启动事件监听go程
	//---------------------------------------------------------------------------------
	go worker.update()

	//--------------------------------------task4--------------------------------------
	//task4:等待agent挖好矿的区块
	//---------------------------------------------------------------------------------
	go worker.wait()

	//--------------------------------------task5--------------------------------------
	//task5:向agent提交一个新挖矿的工作
	//---------------------------------------------------------------------------------
	worker.commitNewWork()

	return worker
}

func (self *worker) setEtherbase(addr common.Address) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.coinbase = addr
}

func (self *worker) setExtra(extra []byte) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.extra = extra
}

func (self *worker) pending() (*types.Block, *state.StateDB) {
	if atomic.LoadInt32(&self.mining) == 0 {
		// return a snapshot to avoid contention on currentMu mutex
		self.snapshotMu.RLock()
		defer self.snapshotMu.RUnlock()
		return self.snapshotBlock, self.snapshotState.Copy()
	}

	self.currentMu.Lock()
	defer self.currentMu.Unlock()
	return self.current.Block, self.current.state.Copy()
}

func (self *worker) pendingBlock() *types.Block {
	if atomic.LoadInt32(&self.mining) == 0 {
		// return a snapshot to avoid contention on currentMu mutex
		self.snapshotMu.RLock()
		defer self.snapshotMu.RUnlock()
		return self.snapshotBlock
	}

	self.currentMu.Lock()
	defer self.currentMu.Unlock()
	return self.current.Block
}

func (self *worker) start() {
	self.mu.Lock()
	defer self.mu.Unlock()

	atomic.StoreInt32(&self.mining, 1)

	// spin up agents
	for agent := range self.agents {
		agent.Start()
	}
}

func (self *worker) stop() {
	self.wg.Wait()

	self.mu.Lock()
	defer self.mu.Unlock()
	if atomic.LoadInt32(&self.mining) == 1 {
		for agent := range self.agents {
			agent.Stop()
		}
	}
	atomic.StoreInt32(&self.mining, 0)
	atomic.StoreInt32(&self.atWork, 0)
}

func (self *worker) register(agent Agent) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.agents[agent] = struct{}{}
	agent.SetReturnCh(self.recv)
}

func (self *worker) unregister(agent Agent) {
	self.mu.Lock()
	defer self.mu.Unlock()
	delete(self.agents, agent)
	agent.Stop()
}

//主要功能:监听交易池和blockChain事件
//task1:接收到规范链更新事件后，提交一个新区块的挖矿工作
//task2:接收到分叉的更新事件后，保存分叉上的区块到可能的父区块列表
//task3:接受到交易池更新事件后，将新加入交易池的交易池应用到pending状态上
func (self *worker) update() {
	defer self.txsSub.Unsubscribe()
	defer self.chainHeadSub.Unsubscribe()
	defer self.chainSideSub.Unsubscribe()

	for {
		// A real event arrived, process interesting content
		select {
		// Handle ChainHeadEvent
		//--------------------------------------task1--------------------------------------
		//task1:接收到规范链更新事件后，提交一个新区块的挖矿工作
		//---------------------------------------------------------------------------------
		case <-self.chainHeadCh:
			self.commitNewWork()

		// Handle ChainSideEvent
		//--------------------------------------task2--------------------------------------
		//task2:接收到分叉的更新事件后，保存分叉上的区块到可能的父区块列表
		//---------------------------------------------------------------------------------
		case ev := <-self.chainSideCh:
			self.uncleMu.Lock()
			self.possibleUncles[ev.Block.Hash()] = ev.Block
			self.uncleMu.Unlock()

		// Handle NewTxsEvent
		//--------------------------------------task2--------------------------------------
		//task3:接受到交易池更新事件后，将新加入交易池的交易池应用到pending状态上
		//---------------------------------------------------------------------------------
		case ev := <-self.txsCh:
			// Apply transactions to the pending state if we're not mining.
			//
			// Note all transactions received may not be continuous with transactions
			// already included in the current mining block. These transactions will
			// be automatically eliminated.
			//如果当前不在挖矿,将收到的交易应用到等待挖矿的区块状态上
			if atomic.LoadInt32(&self.mining) == 0 {
				self.currentMu.Lock()
				txs := make(map[common.Address]types.Transactions)
				//以发送者为key,将交易整理好
				//from1:[tx1, tx2,...]
				//from2:[tx1, tx2,...]
				//...
				for _, tx := range ev.Txs {
					acc, _ := types.Sender(self.current.signer, tx)
					txs[acc] = append(txs[acc], tx)
				}
				//获取每个发送这的第一个交易，组成一个列表，并排序
				txset := types.NewTransactionsByPriceAndNonce(self.current.signer, txs)
				self.current.commitTransactions(self.mux, txset, self.chain, self.coinbase)
				self.updateSnapshot()
				self.currentMu.Unlock()
			} else {//如果正在挖矿，但没有任何事情处理，提交一个新工作
				// If we're mining, but nothing is being processed, wake on new transactions
				if self.config.Clique != nil && self.config.Clique.Period == 0 {
					self.commitNewWork()
				}
			}

		// System stopped
		case <-self.txsSub.Err():
			return
		case <-self.chainHeadSub.Err():
			return
		case <-self.chainSideSub.Err():
			return
		}
	}
}

//主要功能:等待agent返回成功挖矿的区块
//task1:将区块尝试写入规范链
//task2:提交新区快挖矿成功事件
//task3:提交区块链更新事件和规范链更新事件
//task4将新挖出的区块插入未确认的列表
func (self *worker) wait() {
	for {
		for result := range self.recv {
			atomic.AddInt32(&self.atWork, -1)

			if result == nil {
				continue
			}
			block := result.Block
			work := result.Work

			// Update the block hash in all logs since it is now available and not when the
			// receipt/log of individual transactions were created.
			for _, r := range work.receipts {
				for _, l := range r.Logs {
					l.BlockHash = block.Hash()
				}
			}
			for _, log := range work.state.Logs() {
				log.BlockHash = block.Hash()
			}
			//--------------------------------------task1--------------------------------------
			//task1:将区块尝试写入规范链
			//---------------------------------------------------------------------------------
			stat, err := self.chain.WriteBlockWithState(block, work.receipts, work.state)
			if err != nil {
				log.Error("Failed writing block to chain", "err", err)
				continue
			}
			//--------------------------------------task2--------------------------------------
			//task2:提交新区快挖矿成功事件
			//---------------------------------------------------------------------------------
			// Broadcast the block and announce chain insertion event
			self.mux.Post(core.NewMinedBlockEvent{Block: block})
			//--------------------------------------task3--------------------------------------
			//task3:提交区块链更新事件和规范链更新事件
			//---------------------------------------------------------------------------------
			var (
				events []interface{}
				logs   = work.state.Logs()
			)
			events = append(events, core.ChainEvent{Block: block, Hash: block.Hash(), Logs: logs})
			if stat == core.CanonStatTy {
				events = append(events, core.ChainHeadEvent{Block: block})
			}
			self.chain.PostChainEvents(events, logs)

			//--------------------------------------task4--------------------------------------
			//task4将新挖出的区块插入未确认的列表
			//---------------------------------------------------------------------------------
			// Insert the block into the set of pending ones to wait for confirmations
			self.unconfirmed.Insert(block.NumberU64(), block.Hash())
		}
	}
}

// push sends a new work task to currently live miner agents.
func (self *worker) push(work *Work) {
	if atomic.LoadInt32(&self.mining) != 1 {
		return
	}
	for agent := range self.agents {
		atomic.AddInt32(&self.atWork, 1)
		if ch := agent.Work(); ch != nil {
			ch <- work
		}
	}
}

// makeCurrent creates a new environment for the current cycle.
//主要功能:创建当前挖矿周期的工作环境Work
//task1:初始化Work类
//task2:填充Work的有效祖先（ancestors）和家庭成员（family)
func (self *worker) makeCurrent(parent *types.Block, header *types.Header) error {
	state, err := self.chain.StateAt(parent.Root())
	if err != nil {
		return err
	}
	//--------------------------------------task1--------------------------------------
	//task1:初始化Work类
	//---------------------------------------------------------------------------------
	work := &Work{
		config:    self.config,
		signer:    types.NewEIP155Signer(self.config.ChainID),
		state:     state,
		ancestors: set.New(),
		family:    set.New(),
		uncles:    set.New(),
		header:    header,
		createdAt: time.Now(),
	}

	// when 08 is processed ancestors contain 07 (quick block)
	//--------------------------------------task2--------------------------------------
	//task2:填充Work的有效祖先（ancestors）列表和(family)列表
	//---------------------------------------------------------------------------------
	for _, ancestor := range self.chain.GetBlocksFromHash(parent.Hash(), 7) {
		for _, uncle := range ancestor.Uncles() {
			work.family.Add(uncle.Hash())
		}
		work.family.Add(ancestor.Hash())
		work.ancestors.Add(ancestor.Hash())
	}

	// Keep track of transactions which return errors so they can be removed
	work.tcount = 0
	self.current = work
	return nil
}

//主要功能:提交一个新工作给agent
//task1:初始化一个新区块头给待挖矿的区块
//task2:为当前挖矿周期初始化一个工作环境work
//task3:获取交易池中每个账户地址的交易列表中的第一个交易
//task4:获取两个叔块
//task5:用给定的状态来创建新区块，同时计算奖励（旷工奖励和叔块旷工奖励）
//task6:将区块提交给agent
//task7:更新快照
func (self *worker) commitNewWork() {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.uncleMu.Lock()
	defer self.uncleMu.Unlock()
	self.currentMu.Lock()
	defer self.currentMu.Unlock()

	tstart := time.Now()
	parent := self.chain.CurrentBlock()

	//如果父区块的时间比现在的时间还大,将当前时间设置为父亲区块的时间+1
	tstamp := tstart.Unix()
	if parent.Time().Cmp(new(big.Int).SetInt64(tstamp)) >= 0 {
		tstamp = parent.Time().Int64() + 1
	}
	// this will ensure we're not going off too far in the future
	// 如果父区块的时间大于本地时间，我们等一会
	if now := time.Now().Unix(); tstamp > now+1 {
		wait := time.Duration(tstamp-now) * time.Second
		log.Info("Mining too far in the future", "wait", common.PrettyDuration(wait))
		time.Sleep(wait)
	}


	//--------------------------------------task1--------------------------------------
	//task1:初始化一个新区块头给待挖矿的区块
	//---------------------------------------------------------------------------------
	num := parent.Number()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   core.CalcGasLimit(parent),
		Extra:      self.extra,
		Time:       big.NewInt(tstamp),
	}
	// Only set the coinbase if we are mining (avoid spurious block rewards)
	//只有当调用了miner.start()，后才把当前的旷工地址赋值给待挖矿区块头
	if atomic.LoadInt32(&self.mining) == 1 {
		header.Coinbase = self.coinbase
	}
	//计算待挖矿区块的难度值
	if err := self.engine.Prepare(self.chain, header); err != nil {
		log.Error("Failed to prepare header for mining", "err", err)
		return
	}
	//处理dao事件的硬分叉
	// If we are care about TheDAO hard-fork check whether to override the extra-data or not
	if daoBlock := self.config.DAOForkBlock; daoBlock != nil {
		// Check whether the block is among the fork extra-override range
		limit := new(big.Int).Add(daoBlock, params.DAOForkExtraRange)
		if header.Number.Cmp(daoBlock) >= 0 && header.Number.Cmp(limit) < 0 {
			// Depending whether we support or oppose the fork, override differently
			if self.config.DAOForkSupport {
				header.Extra = common.CopyBytes(params.DAOForkBlockExtra)
			} else if bytes.Equal(header.Extra, params.DAOForkBlockExtra) {
				header.Extra = []byte{} // If miner opposes, don't let it use the reserved extra-data
			}
		}
	}
	// Could potentially happen if starting to mine in an odd state.
	//--------------------------------------task2--------------------------------------
	//task2:为当前挖矿周期初始化一个工作环境work
	//---------------------------------------------------------------------------------
	err := self.makeCurrent(parent, header)
	if err != nil {
		log.Error("Failed to create mining context", "err", err)
		return
	}
	// Create the current work task and check any fork transitions needed
	work := self.current
	if self.config.DAOForkSupport && self.config.DAOForkBlock != nil && self.config.DAOForkBlock.Cmp(header.Number) == 0 {
		misc.ApplyDAOHardFork(work.state)
	}
	//--------------------------------------task3--------------------------------------
	//task3:获取交易池中每个账户地址的交易列表中的第一个交易
	//---------------------------------------------------------------------------------
	pending, err := self.eth.TxPool().Pending()
	if err != nil {
		log.Error("Failed to fetch pending transactions", "err", err)
		return
	}
	txs := types.NewTransactionsByPriceAndNonce(self.current.signer, pending)
	work.commitTransactions(self.mux, txs, self.chain, self.coinbase)

	//--------------------------------------task4--------------------------------------
	//task4:获取两个叔块
	//---------------------------------------------------------------------------------
	// compute uncles for the new block.
	var (
		uncles    []*types.Header
		badUncles []common.Hash
	)
	for hash, uncle := range self.possibleUncles {
		if len(uncles) == 2 {
			break
		}
		//挑选出错误的Uncle和2个正确的Uncle
		if err := self.commitUncle(work, uncle.Header()); err != nil {
			log.Trace("Bad uncle found and will be removed", "hash", hash)
			log.Trace(fmt.Sprint(uncle))

			badUncles = append(badUncles, hash)
		} else {
			log.Debug("Committing new uncle to block", "hash", hash)
			uncles = append(uncles, uncle.Header())
		}
	}
	//从可能的uncles列表中剔除那些错误uncle
	for _, hash := range badUncles {
		delete(self.possibleUncles, hash)
	}
	//--------------------------------------task5--------------------------------------
	//task5:用给定的状态来创建新区块，同时计算奖励（旷工奖励和叔块旷工奖励）
	//---------------------------------------------------------------------------------
	// Create the new block to seal with the consensus engine
	if work.Block, err = self.engine.Finalize(self.chain, header, work.state, work.txs, uncles, work.receipts); err != nil {
		log.Error("Failed to finalize block for sealing", "err", err)
		return
	}
	// We only care about logging if we're actually mining.
	if atomic.LoadInt32(&self.mining) == 1 {
		log.Info("Commit new mining work", "number", work.Block.Number(), "txs", work.tcount, "uncles", len(uncles), "elapsed", common.PrettyDuration(time.Since(tstart)))
		self.unconfirmed.Shift(work.Block.NumberU64() - 1)
	}
	//--------------------------------------task6--------------------------------------
	//task6:将区块提交给agent
	//---------------------------------------------------------------------------------
	self.push(work)

	//--------------------------------------task7--------------------------------------
	//task7:更新快照
	//---------------------------------------------------------------------------------
	self.updateSnapshot()
}

func (self *worker) commitUncle(work *Work, uncle *types.Header) error {
	hash := uncle.Hash()
	if work.uncles.Has(hash) {
		return fmt.Errorf("uncle not unique")
	}
	//如果待打包区块的7个祖先区块中没有uncle的父区块,直接退出
	if !work.ancestors.Has(uncle.ParentHash) {
		return fmt.Errorf("uncle's parent unknown (%x)", uncle.ParentHash[0:4])
	}
	//如果待打包区块的家庭成员中已经有这个叔区块，直接退出
	if work.family.Has(hash) {
		return fmt.Errorf("uncle already in family (%x)", hash)
	}
	//将叔区块加入到叔区块列表中
	work.uncles.Add(uncle.Hash())
	return nil
}

func (self *worker) updateSnapshot() {
	self.snapshotMu.Lock()
	defer self.snapshotMu.Unlock()
	self.snapshotBlock = types.NewBlock(
		self.current.header,
		self.current.txs,
		nil,
		self.current.receipts,
	)
	self.snapshotState = self.current.state.Copy()
}

//主要功能:应用交易(执行交易),改变世界状态，向外发送log事件
//task1:交易执行循环
//		  step1 - 如果区块工作环境剩余的gas额度小于21000, 则退出循环
//		  step2 - 从待处理交易列表中取一个交易，如果txs中没有交易，则直接退出循环
//        step3 - 如果交易是被保护的，直接跳过这个交易
//        step4 - 执行交易,并处理执行错误
//        step5 - 处理返回值
//			  1.区块工作环境中的剩余gas不够这个交易的gas消耗,剔除这个交易，goto step1
//			  2.交易nonce值太低，如果当前交易的发送者账户列表中还有交易，则取下一个交易替换处理列表中的第一个交易,重新排序(由高到低) goto step1
//			  3.交易nonce值太高， 将当前交易从txs列表中移除,goto step1
//			  4.一切正常，收集日志，统计执行成功的交易技术, goto step1
//            5.如果当前交易的发送者账户列表中还有交易，则取下一个交易替换处理列表中的第一个交易,并且重新对待处理交易列表进行排序排序(由高到低) goto step1
func (env *Work) commitTransactions(mux *event.TypeMux, txs *types.TransactionsByPriceAndNonce, bc *core.BlockChain, coinbase common.Address) {
	//Work.gasPool没有初始化，就做一次初始化
	if env.gasPool == nil {
		//将Work.gasPool初始化为区块的gasLimt
		env.gasPool = new(core.GasPool).AddGas(env.header.GasLimit)
	}

	var coalescedLogs []*types.Log
	// --------------------------------------task1--------------------------------------
	//task1:交易执行循环
	//----------------------------------------------------------------------------------

	for {
		// If we don't have enough gas for any further transactions then we're done
		// +++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		// step1 - 如果区块工作环境剩余的gas额度小于21000, 则退出循环
		// +++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		if env.gasPool.Gas() < params.TxGas {
			log.Trace("Not enough gas for further transactions", "have", env.gasPool, "want", params.TxGas)
			break
		}
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		// +++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		//step2 - 如果txs中没有交易，则直接退出循环
		// +++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		if tx == nil {
			break
		}
		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		//取出交易接受者
		from, _ := types.Sender(env.signer, tx)
		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		// +++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		//step3 - 如果交易是被保护的，直接跳过这个交易
		// +++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		if tx.Protected() && !env.config.IsEIP155(env.header.Number) {
			log.Trace("Ignoring reply protected transaction", "hash", tx.Hash(), "eip155", env.config.EIP155Block)

			txs.Pop()
			continue
		}
		// Start executing the transaction
		// +++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		//step4 - 执行交易,并处理执行错误
		// +++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		env.state.Prepare(tx.Hash(), common.Hash{}, env.tcount)

		err, logs := env.commitTransaction(tx, bc, coinbase, env.gasPool)
		// +++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		//step5 - 处理返回值
		// +++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		switch err {
		case core.ErrGasLimitReached:
			// Pop the current out-of-gas transaction without shifting in the next from the account
			//区块工作环境中的剩余gas不够这个交易的gas消耗
			log.Trace("Gas limit exceeded for current block", "sender", from)
			//将当前交易从txs列表中移除
			txs.Pop()

		case core.ErrNonceTooLow:
			// New head notification data race between the transaction pool and miner, shift
			log.Trace("Skipping transaction with low nonce", "sender", from, "nonce", tx.Nonce())
			//交易nonce值太低，如果当前交易的发送者账户列表中还有交易，则取下一个交易替换处理列表中的第一个交易,重新排序(由高到低)
			txs.Shift()

		case core.ErrNonceTooHigh:
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Trace("Skipping account with hight nonce", "sender", from, "nonce", tx.Nonce())
			//交易nonce值太高， 将当前交易从txs列表中移除
			txs.Pop()

		case nil:
			// Everything ok, collect the logs and shift in the next transaction from the same account
			//一切正常，收集日志，统计执行成功的交易技术
			coalescedLogs = append(coalescedLogs, logs...)
			env.tcount++
			//如果当前交易的发送者账户列表中还有交易，则取下一个交易替换处理列表中的第一个交易,并且重新对待处理交易列表进行排序排序(由高到低)
			txs.Shift()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			//如果当前交易的发送者账户列表中还有交易，则取下一个交易替换处理列表中的第一个交易,重新排序(由高到低)
			txs.Shift()
		}
	}

	if len(coalescedLogs) > 0 || env.tcount > 0 {
		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		go func(logs []*types.Log, tcount int) {
			if len(logs) > 0 {
				//过滤系统(filter)会监听这个事件
				mux.Post(core.PendingLogsEvent{Logs: logs})
			}
			if tcount > 0 {
				mux.Post(core.PendingStateEvent{})
			}
		}(cpy, env.tcount)
	}
}

//主要功能:应用一个交易（使用EVM执行一个交易）,处理出错情况
//task1:保存应用交易前，区块工作环境的世界状态
//task2:应用交易,如果应用交易出错，直接恢复应用前的状态
//task3:更新区块工作环境中的txs和receipts列表
func (env *Work) commitTransaction(tx *types.Transaction, bc *core.BlockChain, coinbase common.Address, gp *core.GasPool) (error, []*types.Log) {
	//------------------------------------------task1-------------------------------------
	//task1:保存应用交易前，区块工作环境的世界状态
	//--------------------------------------------------------------------------------------
	snap := env.state.Snapshot()

	//------------------------------------------task2-------------------------------------
	//task2:应用交易,如果应用交易出错，直接恢复应用前的状态
	//--------------------------------------------------------------------------------------
	receipt, _, err := core.ApplyTransaction(env.config, bc, &coinbase, gp, env.state, env.header, tx, &env.header.GasUsed, vm.Config{})
	if err != nil {
		env.state.RevertToSnapshot(snap)
		return err, nil
	}
	//------------------------------------------task3-------------------------------------
	//task3:更新区块工作环境中的txs和receipts列表
	//--------------------------------------------------------------------------------------
	env.txs = append(env.txs, tx)
	env.receipts = append(env.receipts, receipt)

	return nil, receipt.Logs
}
