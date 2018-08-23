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

package core

import (
	"errors"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

var (
	errInsufficientBalanceForGas = errors.New("insufficient balance to pay for gas")
)

/*
The State Transitioning Model

A state transition is a change made when a transaction is applied to the current world state
The state transitioning model does all all the necessary work to work out a valid new state root.

1) Nonce handling
2) Pre pay gas
3) Create a new state object if the recipient is \0*32
4) Value transfer
== If contract creation ==
  4a) Attempt to run transaction data
  4b) If valid, use result as code for the new state object
== end ==
5) Run Script section
6) Derive new state root
*/
type StateTransition struct {
	gp         *GasPool //区块工作环境中的gas剩余额度
	msg        Message //交易转化成的message
	gas        uint64  //交易的gas余额
	gasPrice   *big.Int
	initialGas uint64 //初始gas, 等于交易的gaslimit
	value      *big.Int //交易的转账额度
	data       []byte //交易的input
	state      vm.StateDB //状态树
	evm        *vm.EVM   //evm对象
}

// Message represents a message sent to a contract.
type Message interface {
	From() common.Address
	//FromFrontier() (common.Address, error)
	To() *common.Address

	GasPrice() *big.Int
	Gas() uint64
	Value() *big.Int

	Nonce() uint64
	CheckNonce() bool
	Data() []byte
}
//主要功能：计算固定消耗 + 数据存储消耗(合约账户)
//task1: 统计固定消耗
//task2: 统计非0值数据的消耗
//task3: 统计0值数据的消耗
// IntrinsicGas computes the 'intrinsic gas' for a message with the given data.
func IntrinsicGas(data []byte, contractCreation, homestead bool) (uint64, error) {
	// Set the starting gas for the raw transaction
	var gas uint64
	if contractCreation && homestead {
		//如果是合约创建并且是家园版本， 固定消耗为 53000gas
		gas = params.TxGasContractCreation
	} else {
		//如果是交易的话，固定消耗为 21000gas
		gas = params.TxGas
	}
	// Bump the required gas by the amount of transactional data
	if len(data) > 0 {
		// Zero and non-zero bytes are priced differently
		var nz uint64
		for _, byt := range data {
			if byt != 0 {
				nz++
			}
		}
		// Make sure we don't exceed uint64 for all data combinations
		//-------------------------------task1------------------------------------
		//task1: 统计非0值数据的消耗
		//------------------------------------------------------------------------
		//确保    固定消耗+ 非0值的数量×68 <= 64位能表示的最大内存值
		if (math.MaxUint64-gas)/params.TxDataNonZeroGas < nz {
			return 0, vm.ErrOutOfGas
		}
		//每字节68gas
		gas += nz * params.TxDataNonZeroGas

		//-------------------------------task2------------------------------------
		//task3: 统计0值数据的消耗
		//------------------------------------------------------------------------
		//确保   确定消耗 + 0值的数量×4 <= 64位能表示的最大数
		z := uint64(len(data)) - nz
		if (math.MaxUint64-gas)/params.TxDataZeroGas < z {
			return 0, vm.ErrOutOfGas
		}
		gas += z * params.TxDataZeroGas
	}
	return gas, nil
}

// NewStateTransition initialises and returns a new state transition object.
func NewStateTransition(evm *vm.EVM, msg Message, gp *GasPool) *StateTransition {
	return &StateTransition{
		gp:       gp,
		evm:      evm,
		msg:      msg,
		gasPrice: msg.GasPrice(),
		value:    msg.Value(),
		data:     msg.Data(),
		state:    evm.StateDB,
	}
}

// ApplyMessage computes the new state by applying the given message
// against the old state within the environment.
//
// ApplyMessage returns the bytes returned by any EVM execution (if it took place),
// the gas used (which includes gas refunds) and an error if it failed. An error always
// indicates a core error meaning that the message would always fail for that particular
// state and would never be accepted within a block.
func ApplyMessage(evm *vm.EVM, msg Message, gp *GasPool) ([]byte, uint64, bool, error) {
	return NewStateTransition(evm, msg, gp).TransitionDb()
}

// to returns the recipient of the message.
func (st *StateTransition) to() common.Address {
	if st.msg == nil || st.msg.To() == nil /* contract creation */ {
		return common.Address{}
	}
	return *st.msg.To()
}

func (st *StateTransition) useGas(amount uint64) error {
	if st.gas < amount {
		return vm.ErrOutOfGas
	}
	st.gas -= amount

	return nil
}

func (st *StateTransition) buyGas() error {
	mgval := new(big.Int).Mul(new(big.Int).SetUint64(st.msg.Gas()), st.gasPrice)
	//账户里余额够不够支付
	if st.state.GetBalance(st.msg.From()).Cmp(mgval) < 0 {
		return errInsufficientBalanceForGas
	}
	//从Work的gasPool中减去当前交易的额度
	if err := st.gp.SubGas(st.msg.Gas()); err != nil {
		return err
	}
	//设置交易工作环境中的初始gas
	st.gas += st.msg.Gas()

	//初始gas， st.initialGas-st.gas == 剩余部分
	st.initialGas = st.msg.Gas()
	st.state.SubBalance(st.msg.From(), mgval)
	return nil
}

//主要功能:检查nonce值是否正确,然后从区块工作环境中申请交易的gase额度
func (st *StateTransition) preCheck() error {
	// Make sure this transaction's nonce is correct.
	if st.msg.CheckNonce() {
		nonce := st.state.GetNonce(st.msg.From())
		if nonce < st.msg.Nonce() {
			return ErrNonceTooHigh
		} else if nonce > st.msg.Nonce() {
			return ErrNonceTooLow
		}
	}
	return st.buyGas()
}

// TransitionDb will transition the state by applying the current message and
// returning the result including the the used gas. It returns an error if it
// failed. An error indicates a consensus issue.
//主要功能：初始化交易工作环境，执行交易，然后处理交易执行前后的gas增减
//task1: 预先检查nonce和gas值,初始化交易工作环境的gas初始值
//task2: 计算并扣除固定gas消耗
//task3: 调用evm创建或执行交易
//task4: 奖励旷工
func (st *StateTransition) TransitionDb() (ret []byte, usedGas uint64, failed bool, err error) {
	//-------------------------------task1------------------------------------
	//task1: 预先检查nonce和gas值,初始化交易工作环境的gas初始值
	//------------------------------------------------------------------------
	if err = st.preCheck(); err != nil {
		return
	}
	msg := st.msg
	sender := vm.AccountRef(msg.From())
	homestead := st.evm.ChainConfig().IsHomestead(st.evm.BlockNumber)
	contractCreation := msg.To() == nil

	//-------------------------------task2------------------------------------
	//task2: 计算并扣除固定gas消耗
	//------------------------------------------------------------------------
	// Pay intrinsic gas
	//计算固定的GAS消耗 + 非0值消耗的gas + 0值消耗的gas
	gas, err := IntrinsicGas(st.data, contractCreation, homestead)
	if err != nil {
		return nil, 0, false, err
	}
	//从GAS pool总减去上面计算的gas消耗
	if err = st.useGas(gas); err != nil {
		return nil, 0, false, err
	}

	var (
		evm = st.evm
		// vm errors do not effect consensus and are therefor
		// not assigned to err, except for insufficient balance
		// error.
		vmerr error
	)
	//-------------------------------task3------------------------------------
	//task3: 调用evm创建或执行交易
	//------------------------------------------------------------------------
	if contractCreation {
		//如果是合约创建, 调用evm.Create创建合约
		ret, _, st.gas, vmerr = evm.Create(sender, st.data, st.gas, st.value)
	} else {
		// Increment the nonce for the next transaction
		//设置交易发送方的nonce值
		st.state.SetNonce(msg.From(), st.state.GetNonce(sender.Address())+1)
		//如果是交易或者是合约调用，调用evm.Call执行交易
		ret, st.gas, vmerr = evm.Call(sender, st.to(), st.data, st.gas, st.value)
	}
	if vmerr != nil {
		log.Debug("VM returned with error", "err", vmerr)
		// The only possible consensus-error would be if there wasn't
		// sufficient balance to make the transfer happen. The first
		// balance transfer may never fail.
		if vmerr == vm.ErrInsufficientBalance {
			return nil, 0, false, vmerr
		}
	}
	//-------------------------------task4------------------------------------
	//task4: 奖励旷工
	//------------------------------------------------------------------------
	//返回余额给交易发起方
	st.refundGas()
	//奖励旷工，gas消耗
	st.state.AddBalance(st.evm.Coinbase, new(big.Int).Mul(new(big.Int).SetUint64(st.gasUsed()), st.gasPrice))

	return ret, st.gasUsed(), vmerr != nil, err
}

//主要功能：返还gas
//task1: 计算当前交易的gas余额
//task2: 增加世界状态状态中的账户余额
//task3: 增加工作环境剩余余额
func (st *StateTransition) refundGas() {
	// Apply refund counter, capped to half of the used gas.
	//-------------------------------task1------------------------------------
	//task1: 计算当前交易的gas总余额, 总余额 = 余额 + 系统"返利"
	//------------------------------------------------------------------------
	refund := st.gasUsed() / 2
	if refund > st.state.GetRefund() {
		refund = st.state.GetRefund()
	}
	st.gas += refund

	//-------------------------------task2------------------------------------
	//task2: 增加世界状态状态中的账户余额
	//------------------------------------------------------------------------
	// Return ETH for remaining gas, exchanged at the original rate.
	remaining := new(big.Int).Mul(new(big.Int).SetUint64(st.gas), st.gasPrice)
	st.state.AddBalance(st.msg.From(), remaining)

	// Also return remaining gas to the block gas counter so it is
	// available for the next transaction.
	//-------------------------------task3------------------------------------
	//task3: 增加工作环境剩余余额
	//------------------------------------------------------------------------
	st.gp.AddGas(st.gas)
}

// gasUsed returns the amount of gas used up by the state transition.
func (st *StateTransition) gasUsed() uint64 {
	return st.initialGas - st.gas
}
