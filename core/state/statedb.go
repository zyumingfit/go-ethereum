// Copyright 2014 The go-ethereum Authors
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

// Package state provides a caching layer atop the Ethereum state trie.
package state

import (
	"fmt"
	"math/big"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

type revision struct {
	id           int
	journalIndex int
}

var (
	// emptyState is the known hash of an empty state trie entry.
	emptyState = crypto.Keccak256Hash(nil)

	// emptyCode is the known hash of the empty EVM bytecode.
	emptyCode = crypto.Keccak256Hash(nil)
)

// StateDBs within the ethereum protocol are used to store anything
// within the merkle trie. StateDBs take care of caching and storing
// nested states. It's the general query interface to retrieve:
// * Contracts
// * Accounts
type StateDB struct {
	db   Database  //数据库
	trie Trie      //MPT树

	// This map holds 'live' objects, which will get modified while processing a state transition.
	stateObjects      map[common.Address]*stateObject //账户状态缓存
	stateObjectsDirty map[common.Address]struct{}     //脏账户表，账户变更缓存

	// DB error.
	// State objects are used by the consensus core and VM which are
	// unable to deal with database-level errors. Any error that occurs
	// during a database read is memoized here and will eventually be returned
	// by StateDB.Commit.
	dbErr error

	// The refund counter, also used by state transitioning.
	refund uint64    //返利 non 0 -> 0  24000
				     //返利 Suicide -> 0  15000

	thash, bhash common.Hash
	txIndex      int
	logs         map[common.Hash][]*types.Log
	logSize      uint

	preimages map[common.Hash][]byte

	// Journal of state modifications. This is the backbone of
	// Snapshot and RevertToSnapshot.
	journal        *journal  //变更日志列表
	validRevisions []revision  // revision{
	                           //      id int            /id == nexRevisionId
	                           //      journalIndex int /*journalIndex == len(journal)*/
	                           //}
	                           //
	nextRevisionId int   //快照计数器

	lock sync.Mutex
}

//主要功能：使用给定的树根创建一个世界状态
//task1: 使用给定的树根构建状态树
//task2:实例化StateDB类
// Create a new state from a given trie.
func New(root common.Hash, db Database) (*StateDB, error) {
	//-------------------------------task1------------------------------------
	//task1: 使用给定的树根构建状态树
	//------------------------------------------------------------------------
	tr, err := db.OpenTrie(root)
	if err != nil {
		return nil, err
	}
	//-------------------------------task2------------------------------------
	//task2:实例化StateDB类
	//------------------------------------------------------------------------
	return &StateDB{
		db:                db,
		trie:              tr,
		stateObjects:      make(map[common.Address]*stateObject),
		stateObjectsDirty: make(map[common.Address]struct{}),
		logs:              make(map[common.Hash][]*types.Log),
		preimages:         make(map[common.Hash][]byte),
		journal:           newJournal(),
	}, nil
}

// setError remembers the first non-nil error it is called with.
func (self *StateDB) setError(err error) {
	if self.dbErr == nil {
		self.dbErr = err
	}
}

func (self *StateDB) Error() error {
	return self.dbErr
}

// Reset clears out all ephemeral state objects from the state db, but keeps
// the underlying state trie to avoid reloading data for the next operations.
func (self *StateDB) Reset(root common.Hash) error {
	tr, err := self.db.OpenTrie(root)
	if err != nil {
		return err
	}
	self.trie = tr
	self.stateObjects = make(map[common.Address]*stateObject)
	self.stateObjectsDirty = make(map[common.Address]struct{})
	self.thash = common.Hash{}
	self.bhash = common.Hash{}
	self.txIndex = 0
	self.logs = make(map[common.Hash][]*types.Log)
	self.logSize = 0
	self.preimages = make(map[common.Hash][]byte)
	self.clearJournalAndRefund()
	return nil
}

func (self *StateDB) AddLog(log *types.Log) {
	self.journal.append(addLogChange{txhash: self.thash})

	log.TxHash = self.thash
	log.BlockHash = self.bhash
	log.TxIndex = uint(self.txIndex)
	log.Index = self.logSize
	self.logs[self.thash] = append(self.logs[self.thash], log)
	self.logSize++
}

func (self *StateDB) GetLogs(hash common.Hash) []*types.Log {
	return self.logs[hash]
}

func (self *StateDB) Logs() []*types.Log {
	var logs []*types.Log
	for _, lgs := range self.logs {
		logs = append(logs, lgs...)
	}
	return logs
}

// AddPreimage records a SHA3 preimage seen by the VM.
func (self *StateDB) AddPreimage(hash common.Hash, preimage []byte) {
	if _, ok := self.preimages[hash]; !ok {
		self.journal.append(addPreimageChange{hash: hash})
		pi := make([]byte, len(preimage))
		copy(pi, preimage)
		self.preimages[hash] = pi
	}
}

// Preimages returns a list of SHA3 preimages that have been submitted.
func (self *StateDB) Preimages() map[common.Hash][]byte {
	return self.preimages
}

func (self *StateDB) AddRefund(gas uint64) {
	self.journal.append(refundChange{prev: self.refund})
	self.refund += gas
}

// Exist reports whether the given account address exists in the state.
// Notably this also returns true for suicided accounts.
func (self *StateDB) Exist(addr common.Address) bool {
	return self.getStateObject(addr) != nil
}

// Empty returns whether the state object is either non-existent
// or empty according to the EIP161 specification (balance = nonce = code = 0)
func (self *StateDB) Empty(addr common.Address) bool {
	so := self.getStateObject(addr)
	return so == nil || so.empty()
}

// Retrieve the balance from the given address or 0 if object not found
//主要功能：获取一个账户的余额
//task1: 获取账户对象stateObject
//task2: 返回账户余额
func (self *StateDB) GetBalance(addr common.Address) *big.Int {
	//-------------------------------task1------------------------------------
	//task1: 获取账户对象stateObject
	//------------------------------------------------------------------------
	stateObject := self.getStateObject(addr)
	//-------------------------------task1------------------------------------
	//task2: 返回账户余额
	//------------------------------------------------------------------------
	if stateObject != nil {
		return stateObject.Balance()
	}
	return common.Big0
}

func (self *StateDB) GetNonce(addr common.Address) uint64 {
	stateObject := self.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Nonce()
	}

	return 0
}

func (self *StateDB) GetCode(addr common.Address) []byte {
	stateObject := self.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Code(self.db)
	}
	return nil
}

func (self *StateDB) GetCodeSize(addr common.Address) int {
	stateObject := self.getStateObject(addr)
	if stateObject == nil {
		return 0
	}
	if stateObject.code != nil {
		return len(stateObject.code)
	}
	size, err := self.db.ContractCodeSize(stateObject.addrHash, common.BytesToHash(stateObject.CodeHash()))
	if err != nil {
		self.setError(err)
	}
	return size
}

func (self *StateDB) GetCodeHash(addr common.Address) common.Hash {
	stateObject := self.getStateObject(addr)
	if stateObject == nil {
		return common.Hash{}
	}
	return common.BytesToHash(stateObject.CodeHash())
}

func (self *StateDB) GetState(addr common.Address, bhash common.Hash) common.Hash {
	stateObject := self.getStateObject(addr)
	if stateObject != nil {
		return stateObject.GetState(self.db, bhash)
	}
	return common.Hash{}
}

// Database retrieves the low level database supporting the lower level trie ops.
func (self *StateDB) Database() Database {
	return self.db
}

// StorageTrie returns the storage trie of an account.
// The return value is a copy and is nil for non-existent accounts.
func (self *StateDB) StorageTrie(addr common.Address) Trie {
	stateObject := self.getStateObject(addr)
	if stateObject == nil {
		return nil
	}
	cpy := stateObject.deepCopy(self)
	return cpy.updateTrie(self.db)
}

func (self *StateDB) HasSuicided(addr common.Address) bool {
	stateObject := self.getStateObject(addr)
	if stateObject != nil {
		return stateObject.suicided
	}
	return false
}

/*
 * SETTERS
 */

// AddBalance adds amount to the account associated with addr.
//主要功能：对一个账户添加余额
//task1: 获取账户对象stateObject
//task2:增加账户中的余额
func (self *StateDB) AddBalance(addr common.Address, amount *big.Int) {
	//-------------------------------task1------------------------------------
	//task1: 获取账户对象stateObject
	//------------------------------------------------------------------------
	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
	//-------------------------------task2------------------------------------
	//task2:增加账户中的余额
	//------------------------------------------------------------------------
		stateObject.AddBalance(amount)
	}
}

// SubBalance subtracts amount from the account associated with addr.
func (self *StateDB) SubBalance(addr common.Address, amount *big.Int) {
	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SubBalance(amount)
	}
}

func (self *StateDB) SetBalance(addr common.Address, amount *big.Int) {
	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetBalance(amount)
	}
}

func (self *StateDB) SetNonce(addr common.Address, nonce uint64) {
	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetNonce(nonce)
	}
}

func (self *StateDB) SetCode(addr common.Address, code []byte) {
	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		stateObject.SetCode(crypto.Keccak256Hash(code), code)
	}
}
//主要功能：设置账户的storage状态
//task1: 获取账户的sateObject对象
//task2: 设置状态
func (self *StateDB) SetState(addr common.Address, key, value common.Hash) {
	//-------------------------------task1------------------------------------
	//task1: 获取账户的sateObject对象
	//------------------------------------------------------------------------
	stateObject := self.GetOrNewStateObject(addr)
	if stateObject != nil {
		//-------------------------------task2------------------------------------
		//task2: 设置状态
		//------------------------------------------------------------------------
		stateObject.SetState(self.db, key, value)
	}
}

// Suicide marks the given account as suicided.
// This clears the account balance.
//
// The account's state object is still available until the state is committed,
// getStateObject will return a non-nil account after Suicide.
//主要功能：销毁账户
//task1: 获取账户的sateObject对象
//task2:添加删除日志
//task3:标记账户自杀,余额清零
func (self *StateDB) Suicide(addr common.Address) bool {
	//-------------------------------task1------------------------------------
	//task1: 获取账户的sateObject对象
	//------------------------------------------------------------------------
	stateObject := self.getStateObject(addr)
	if stateObject == nil {
		return false
	}
	//-------------------------------task2------------------------------------
	//task2:添加删除日志
	//------------------------------------------------------------------------
	self.journal.append(suicideChange{
		account:     &addr,
		prev:        stateObject.suicided,
		prevbalance: new(big.Int).Set(stateObject.Balance()),
	})
	//-------------------------------task3------------------------------------
	//task3:标记账户自杀,余额清零
	//------------------------------------------------------------------------
	stateObject.markSuicided()
	stateObject.data.Balance = new(big.Int)

	return true
}

//
// Setting, updating & deleting state object methods.
//

// updateStateObject writes the given object to the trie.
func (self *StateDB) updateStateObject(stateObject *stateObject) {
	addr := stateObject.Address()
	data, err := rlp.EncodeToBytes(stateObject)
	if err != nil {
		panic(fmt.Errorf("can't encode object at %x: %v", addr[:], err))
	}
	self.setError(self.trie.TryUpdate(addr[:], data))
}

// deleteStateObject removes the given object from the state trie.
func (self *StateDB) deleteStateObject(stateObject *stateObject) {
	stateObject.deleted = true
	addr := stateObject.Address()
	self.setError(self.trie.TryDelete(addr[:]))
}

// Retrieve a state object given by the address. Returns nil if not found.
func (self *StateDB) getStateObject(addr common.Address) (stateObject *stateObject) {
	// Prefer 'live' objects.
	if obj := self.stateObjects[addr]; obj != nil {
		if obj.deleted {
			return nil
		}
		return obj
	}

	// Load the object from the database.
	enc, err := self.trie.TryGet(addr[:])
	if len(enc) == 0 {
		self.setError(err)
		return nil
	}
	var data Account
	if err := rlp.DecodeBytes(enc, &data); err != nil {
		log.Error("Failed to decode state object", "addr", addr, "err", err)
		return nil
	}
	// Insert into the live set.
	obj := newObject(self, addr, data)
	self.setStateObject(obj)
	return obj
}

func (self *StateDB) setStateObject(object *stateObject) {
	self.stateObjects[object.Address()] = object
}

// Retrieve a state object or create a new state object if nil.
func (self *StateDB) GetOrNewStateObject(addr common.Address) *stateObject {
	//从缓存中获取有没有给定地址的账户
	stateObject := self.getStateObject(addr)
	//如果缓存中没有，或者缓存中的账户标示为已经删除了,则重新创建一个账户
	if stateObject == nil || stateObject.deleted {
		stateObject, _ = self.createObject(addr)
	}
	return stateObject
}

// createObject creates a new state object. If there is an existing account with
// the given address, it is overwritten and returned as the second return value.
//主要功能：创建一个新账户
//task1: 实例化一个stateObject对象
//task2: 在日志中添加创建新账户事件
//task3:将新账户添加到缓存中
func (self *StateDB) createObject(addr common.Address) (newobj, prev *stateObject) {
	prev = self.getStateObject(addr)
	//-------------------------------task1------------------------------------
	//task1: 实例化一个stateObject对象
	//------------------------------------------------------------------------
	newobj = newObject(self, addr, Account{})
	//设置初始的nonce值
	newobj.setNonce(0) // sets the object to dirty
	//-------------------------------task2------------------------------------
	//task2: 在日志中添加创建新账户事件
	//------------------------------------------------------------------------
	if prev == nil {
		self.journal.append(createObjectChange{account: &addr})
	} else {
		self.journal.append(resetObjectChange{prev: prev})
	}
	//-------------------------------task3------------------------------------
	//task3:将新账户添加到缓存中
	//------------------------------------------------------------------------
	self.setStateObject(newobj)
	return newobj, prev
}

// CreateAccount explicitly creates a state object. If a state object with the address
// already exists the balance is carried over to the new account.
//
// CreateAccount is called during the EVM CREATE operation. The situation might arise that
// a contract does the following:
//
//   1. sends funds to sha(account ++ (nonce + 1))
//   2. tx_create(sha(account ++ nonce)) (note that this gets the address of 1)
//
// Carrying over the balance ensures that Ether doesn't disappear.
func (self *StateDB) CreateAccount(addr common.Address) {
	new, prev := self.createObject(addr)
	if prev != nil {
		new.setBalance(prev.data.Balance)
	}
}

func (db *StateDB) ForEachStorage(addr common.Address, cb func(key, value common.Hash) bool) {
	so := db.getStateObject(addr)
	if so == nil {
		return
	}

	// When iterating over the storage check the cache first
	for h, value := range so.cachedStorage {
		cb(h, value)
	}

	it := trie.NewIterator(so.getTrie(db.db).NodeIterator(nil))
	for it.Next() {
		// ignore cached values
		key := common.BytesToHash(db.trie.GetKey(it.Key))
		if _, ok := so.cachedStorage[key]; !ok {
			cb(key, common.BytesToHash(it.Value))
		}
	}
}

// Copy creates a deep, independent copy of the state.
// Snapshots of the copied state cannot be applied to the copy.
//主要功能：状态树拷贝
//task1:实例化一个新的StateDB
//task2:拷贝账户缓存列表
//task3:拷贝脏账户缓存列表
//task4:拷贝日志
//task5:拷贝sha3原像
func (self *StateDB) Copy() *StateDB {
	self.lock.Lock()
	defer self.lock.Unlock()

	// Copy all the basic fields, initialize the memory ones
	//-------------------------------task1------------------------------------
	//task1:实例化一个新的StateDB
	//------------------------------------------------------------------------
	state := &StateDB{
		db:                self.db,
		trie:              self.db.CopyTrie(self.trie),
		stateObjects:      make(map[common.Address]*stateObject, len(self.journal.dirties)),
		stateObjectsDirty: make(map[common.Address]struct{}, len(self.journal.dirties)),
		refund:            self.refund,
		logs:              make(map[common.Hash][]*types.Log, len(self.logs)),
		logSize:           self.logSize,
		preimages:         make(map[common.Hash][]byte),
		journal:           newJournal(),
	}
	//-------------------------------task2------------------------------------
	//task2:拷贝账户缓存列表
	//------------------------------------------------------------------------
	// Copy the dirty states, logs, and preimages
	for addr := range self.journal.dirties {
		// As documented [here](https://github.com/ethereum/go-ethereum/pull/16485#issuecomment-380438527),
		// and in the Finalise-method, there is a case where an object is in the journal but not
		// in the stateObjects: OOG after touch on ripeMD prior to Byzantium. Thus, we need to check for
		// nil
		if object, exist := self.stateObjects[addr]; exist {
			state.stateObjects[addr] = object.deepCopy(state)
			state.stateObjectsDirty[addr] = struct{}{}
		}
	}
	// Above, we don't copy the actual journal. This means that if the copy is copied, the
	// loop above will be a no-op, since the copy's journal is empty.
	// Thus, here we iterate over stateObjects, to enable copies of copies
	//-------------------------------task3------------------------------------
	//task3:拷贝脏账户缓存列表
	//------------------------------------------------------------------------
	for addr := range self.stateObjectsDirty {
		if _, exist := state.stateObjects[addr]; !exist {
			state.stateObjects[addr] = self.stateObjects[addr].deepCopy(state)
			state.stateObjectsDirty[addr] = struct{}{}
		}
	}

	//-------------------------------task4------------------------------------
	//task4:拷贝日志
	//------------------------------------------------------------------------
	for hash, logs := range self.logs {
		state.logs[hash] = make([]*types.Log, len(logs))
		copy(state.logs[hash], logs)
	}
	//-------------------------------task5------------------------------------
	//task5:拷贝sha3原像
	//------------------------------------------------------------------------
	for hash, preimage := range self.preimages {
		state.preimages[hash] = preimage
	}
	return state
}

// Snapshot returns an identifier for the current revision of the state.
//主要功能：拍摄快照
//task1:获取快照id,从0开始计数
//task2:保存快照，快照id和日志深度
func (self *StateDB) Snapshot() int {
	//-------------------------------task1------------------------------------
	//task1:获取快照id,从0开始计数
	//------------------------------------------------------------------------
	id := self.nextRevisionId
	self.nextRevisionId++
	//-------------------------------task2------------------------------------
	//task2:保存快照
	//------------------------------------------------------------------------
	//快照包括快照id和日志的深度
	self.validRevisions = append(self.validRevisions, revision{id, self.journal.length()})
	return id
}

//主要功能：恢复快照
//task1:检查快照编号是否有效，如果idx无效，则终止程序
//task2:通过快照编号获取日志长度
//task3:调用日志中的undo函数进行恢复
//task4:移除恢复点后面的快照
// RevertToSnapshot reverts all state changes made since the given revision.
func (self *StateDB) RevertToSnapshot(revid int) {
	// Find the snapshot in the stack of valid snapshots.
	//-------------------------------task1------------------------------------
	//task1:检查快照编号是否有效，如果idx无效，则终止程序
	//------------------------------------------------------------------------
	idx := sort.Search(len(self.validRevisions), func(i int) bool {
		return self.validRevisions[i].id >= revid
	})
	if idx == len(self.validRevisions) || self.validRevisions[idx].id != revid {
		panic(fmt.Errorf("revision id %v cannot be reverted", revid))
	}
	//-------------------------------task2------------------------------------
	//task2:通过快照编号获取日志长度
	//------------------------------------------------------------------------
	//当前快照的日志长度
	snapshot := self.validRevisions[idx].journalIndex

	//-------------------------------task3------------------------------------
	//task3:调用日志中的undo函数进行恢复
	//------------------------------------------------------------------------
	// Replay the journal to undo changes and remove invalidated snapshots
	//注意传入的是快照长度不是索引
	self.journal.revert(self, snapshot)
	//-------------------------------task4------------------------------------
	//task4:移除恢复点后面的快照
	//------------------------------------------------------------------------
	self.validRevisions = self.validRevisions[:idx]
}

// GetRefund returns the current value of the refund counter.
func (self *StateDB) GetRefund() uint64 {
	return self.refund
}

// Finalise finalises the state by removing the self destructed objects
// and clears the journal as well as the refunds.
//主要功能：根据操作日志，更状态树，同时删除快照等信息
//task1:遍历被更新的账户, 将被更新的账户写入状态树
//		step1: 确保账户缓存列表里也存在,否则跳过
//		step2: 将账户的storage变更写入storage树，然后将账户写入状态树
//		step3:记录那些账户被更新过
//task2:清除日志信息、快照信息、返利信息
func (s *StateDB) Finalise(deleteEmptyObjects bool) {
	//-------------------------------task1------------------------------------
	//task1:遍历被更新的账户, 将被更新的账户写入状态树
	//------------------------------------------------------------------------
	for addr := range s.journal.dirties {
		//++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		//step1: 确保账户缓存列表里也存在,否则跳过
		//++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		//确保账户缓存列表里也存在,否则跳过
		stateObject, exist := s.stateObjects[addr]
		if !exist {
			// ripeMD is 'touched' at block 1714175, in tx 0x1237f737031e40bcde4a8b7e717b2d15e3ecadfe49bb1bbc71ee9deb09c6fcf2
			// That tx goes out of gas, and although the notion of 'touched' does not exist there, the
			// touch-event will still be recorded in the journal. Since ripeMD is a special snowflake,
			// it will persist in the journal even though the journal is reverted. In this special circumstance,
			// it may exist in `s.journal.dirties` but not in `s.stateObjects`.
			// Thus, we can safely ignore it here
			continue
		}

		//++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		//step2: 将账户的storage变更写入storage树，然后将账户写入状态树
		//++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		//如果账户已经销毁了,从状态树中删除账户
		//如果账户为空，并且deleteEmptyObjects标志为true,从状态树中删除账户
		if stateObject.suicided || (deleteEmptyObjects && stateObject.empty()) {
			s.deleteStateObject(stateObject)
		} else {
			//将账户的storage变更写入storage树,更新状态树中的storage树根
			stateObject.updateRoot(s.db)
			//将当前账户写入状态树
			s.updateStateObject(stateObject)
		}
		//++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		//step3:记录那些账户被更新过
		//++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
		s.stateObjectsDirty[addr] = struct{}{}
	}
	// Invalidate journal because reverting across transactions is not allowed.
	//-------------------------------task2------------------------------------
	//task2:清除日志信息、快照信息、返利信息
	//------------------------------------------------------------------------
	s.clearJournalAndRefund()
}

// IntermediateRoot computes the current root hash of the state trie.
// It is called in between transactions to get the root hash that
// goes into transaction receipts.
//主要功能：更新状态树，同时计算状态树根
//task1:遍历被更新的账户, 将被更新的账户写入状态树,清除变更日志、快照、返利
//task2:计算状态树树根
func (s *StateDB) IntermediateRoot(deleteEmptyObjects bool) common.Hash {
	//-------------------------------task1------------------------------------
	//task1:遍历被更新的账户, 将被更新的账户写入状态树,清除变更日志、快照、返利
	//------------------------------------------------------------------------
	s.Finalise(deleteEmptyObjects)
	//-------------------------------task1------------------------------------
	//task2:计算状态树树根
	//------------------------------------------------------------------------
	return s.trie.Hash()
}

// Prepare sets the current transaction hash and index and block hash which is
// used when the EVM emits new state logs.
func (self *StateDB) Prepare(thash, bhash common.Hash, ti int) {
	self.thash = thash
	self.bhash = bhash
	self.txIndex = ti
}

func (s *StateDB) clearJournalAndRefund() {
	s.journal = newJournal()
	s.validRevisions = s.validRevisions[:0]
	s.refund = 0
}

// Commit writes the state to the underlying in-memory trie database.
//主要功能：将状态树写入数据库
//task1:更新脏账户表
//task2:遍历被更新的账户, 将被更新的账户写入状态树
//task3:将状态树写入数据库
func (s *StateDB) Commit(deleteEmptyObjects bool) (root common.Hash, err error) {
	defer s.clearJournalAndRefund()

	//-------------------------------task1------------------------------------
	//task1:遍历日志列表，更新脏账户表
	//------------------------------------------------------------------------
	for addr := range s.journal.dirties {
		s.stateObjectsDirty[addr] = struct{}{}
	}
	// Commit objects to the trie.
	//-------------------------------task2------------------------------------
	//task2:遍历被更新的账户, 将被更新的账户写入状态树
	//------------------------------------------------------------------------
	for addr, stateObject := range s.stateObjects {
		_, isDirty := s.stateObjectsDirty[addr]
		switch {
		case stateObject.suicided || (isDirty && deleteEmptyObjects && stateObject.empty()):
			// If the object has been removed, don't bother syncing it
			// and just mark it for deletion in the trie.
			//如果账户已经销毁了,从状态树中删除账户
			//如果账户为空，并且deleteEmptyObjects标志为true,从状态树中删除账户
			s.deleteStateObject(stateObject)
		case isDirty:
			// Write any contract code associated with the state object
			if stateObject.code != nil && stateObject.dirtyCode {
				s.db.TrieDB().Insert(common.BytesToHash(stateObject.CodeHash()), stateObject.code)
				stateObject.dirtyCode = false
			}
			// Write any storage changes in the state object to its storage trie.
			//更新storage树
			if err := stateObject.CommitTrie(s.db); err != nil {
				return common.Hash{}, err
			}
			// Update the object in the main account trie.
			//更新状态树
			s.updateStateObject(stateObject)
		}

		//删除脏账户列表
		delete(s.stateObjectsDirty, addr)
	}
	//-------------------------------task3------------------------------------
	//task3:将状态树写入数据库
	//------------------------------------------------------------------------
	// Write trie changes.
	root, err = s.trie.Commit(func(leaf []byte, parent common.Hash) error {
		var account Account
		if err := rlp.DecodeBytes(leaf, &account); err != nil {
			return nil
		}
		if account.Root != emptyState {
			s.db.TrieDB().Reference(account.Root, parent)
		}
		code := common.BytesToHash(account.CodeHash)
		if code != emptyCode {
			s.db.TrieDB().Reference(code, parent)
		}
		return nil
	})
	log.Debug("Trie cache stats after commit", "misses", trie.CacheMisses(), "unloads", trie.CacheUnloads())
	return root, err
}
