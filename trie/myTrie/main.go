package main

import (
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/common"
	"fmt"
)

func main(){
	diskdb := ethdb.NewMemDatabase()
	triedb := trie.NewDatabase(diskdb)
	t, err:= trie.New(common.Hash{}, triedb)
	if err != nil{
		fmt.Println(err)
		return
	}
	t.TryUpdate([]byte{1, 2}, []byte{0})
	t.TryUpdate([]byte{1, 2, 3}, []byte{1})



}
