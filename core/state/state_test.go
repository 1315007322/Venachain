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

package state

import (
	"bytes"
	"fmt"
	"math/big"
	"testing"

	"github.com/Venachain/Venachain/trie"

	"github.com/Venachain/Venachain/common"
	"github.com/Venachain/Venachain/crypto"
	"github.com/Venachain/Venachain/ethdb"
	checker "gopkg.in/check.v1"
)

type StateSuite struct {
	db    *ethdb.MemDatabase
	state *StateDB
}

var _ = checker.Suite(&StateSuite{})

var toAddr = common.BytesToAddress

func (s *StateSuite) TestDump(c *checker.C) {
	// generate a few entries
	obj1 := s.state.GetOrNewStateObject(toAddr([]byte{0x01}))
	obj1.AddBalance(big.NewInt(22))
	obj2 := s.state.GetOrNewStateObject(toAddr([]byte{0x01, 0x02}))
	obj2.SetCode(crypto.Keccak256Hash([]byte{3, 3, 3, 3, 3, 3, 3}), []byte{3, 3, 3, 3, 3, 3, 3})
	obj3 := s.state.GetOrNewStateObject(toAddr([]byte{0x02}))
	obj3.SetBalance(big.NewInt(44))

	// write some of them to the trie
	s.state.updateStateObject(obj1)
	s.state.updateStateObject(obj2)
	s.state.Commit(false)

	// check that dump contains the state objects that are in trie
	got := string(s.state.Dump())
	want := `{
    "root": "5abc758ddba5a92643ad8d50b682f3b2da40b550de1102b6d37459261dfc3861",
    "accounts": {
        "0000000000000000000000000000000000000001": {
            "balance": "22",
            "nonce": 0,
            "root": "56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
            "codeHash": "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
            "code": "",
            "storage": {}
        },
        "0000000000000000000000000000000000000002": {
            "balance": "44",
            "nonce": 0,
            "root": "56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
            "codeHash": "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
            "code": "",
            "storage": {}
        },
        "0000000000000000000000000000000000000102": {
            "balance": "0",
            "nonce": 0,
            "root": "56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
            "codeHash": "87874902497a5bb968da31a2998d8f22e949d1ef6214bcdedd8bae24cca4b9e3",
            "code": "03030303030303",
            "storage": {}
        }
    }
}`
	if got != want {
		c.Errorf("dump mismatch:\ngot: %s\nwant: %s\n", got, want)
	}
}

func (s *StateSuite) SetUpTest(c *checker.C) {
	s.db = ethdb.NewMemDatabase()
	s.state, _ = New(common.Hash{}, NewDatabase(s.db))
}

func (s *StateSuite) TestNull(c *checker.C) {
	address := common.HexToAddress("0x823140710bf13990e4500136726d8b55")
	s.state.CreateAccount(address)
	//value := common.FromHex("0x823140710bf13990e4500136726d8b55")
	//value := nil
	key := []byte{}

	//s.state.SetState(address, common.Hash{}, value)
	s.state.SetState(address, key, nil)
	s.state.Commit(false)

	if value := s.state.GetState(address, common.Hash{}.Bytes()); bytes.Compare(value, []byte{}) != 0 {
		c.Errorf("expected empty current value, got %x", value)
	}
	if value := s.state.GetCommittedState(address, key); !bytes.Equal(value, []byte{}) {
		c.Errorf("expected empty committed value, got %x", value)
	}
}

func (s *StateSuite) TestSnapshot(c *checker.C) {
	stateobjaddr := toAddr([]byte("aa"))
	var storageaddr common.Hash
	data1 := common.BytesToHash([]byte{42})
	data2 := common.BytesToHash([]byte{43})

	// snapshot the genesis state
	genesis := s.state.Snapshot()

	// set initial state object value
	s.state.SetState(stateobjaddr, storageaddr.Bytes(), data1.Bytes())
	snapshot := s.state.Snapshot()

	// set a new state object value, revert it and ensure correct content
	s.state.SetState(stateobjaddr, storageaddr.Bytes(), data2.Bytes())
	s.state.RevertToSnapshot(snapshot)

	c.Assert(s.state.GetState(stateobjaddr, storageaddr.Bytes()), checker.DeepEquals, data1.Bytes())
	c.Assert(s.state.GetCommittedState(stateobjaddr, storageaddr.Bytes()), checker.DeepEquals, []byte(nil))

	// revert up to the genesis state and ensure correct content
	s.state.RevertToSnapshot(genesis)
	c.Assert(s.state.GetState(stateobjaddr, storageaddr.Bytes()), checker.DeepEquals, []byte{})
	c.Assert(s.state.GetCommittedState(stateobjaddr, storageaddr.Bytes()), checker.DeepEquals, []byte{})
}

func (s *StateSuite) TestSnapshotEmpty(c *checker.C) {
	s.state.RevertToSnapshot(s.state.Snapshot())
}

// use testing instead of checker because checker does not support
// printing/logging in tests (-check.vv does not work)
func TestSnapshot2(t *testing.T) {
	state, _ := New(common.Hash{}, NewDatabase(ethdb.NewMemDatabase()))

	stateobjaddr0 := toAddr([]byte("so0"))
	stateobjaddr1 := toAddr([]byte("so1"))
	var storageaddr common.Address

	data0 := common.BytesToHash([]byte{17})
	data1 := common.BytesToHash([]byte{18})

	state.SetState(stateobjaddr0, storageaddr.Bytes(), data0.Bytes())
	state.SetState(stateobjaddr1, storageaddr.Bytes(), data1.Bytes())

	// db, trie are already non-empty values
	so0 := state.getStateObject(stateobjaddr0)
	so0.SetBalance(big.NewInt(42))
	so0.SetNonce(43)
	so0.SetCode(crypto.Keccak256Hash([]byte{'c', 'a', 'f', 'e'}), []byte{'c', 'a', 'f', 'e'})
	so0.suicided = false
	so0.deleted = false
	state.setStateObject(so0)

	root, _ := state.Commit(false)
	state.Reset(root)

	// and one with deleted == true
	so1 := state.getStateObject(stateobjaddr1)
	so1.SetBalance(big.NewInt(52))
	so1.SetNonce(53)
	so1.SetCode(crypto.Keccak256Hash([]byte{'c', 'a', 'f', 'e', '2'}), []byte{'c', 'a', 'f', 'e', '2'})
	so1.suicided = true
	so1.deleted = true
	state.setStateObject(so1)

	so1 = state.getStateObject(stateobjaddr1)
	if so1 != nil {
		t.Fatalf("deleted object not nil when getting")
	}

	snapshot := state.Snapshot()
	state.RevertToSnapshot(snapshot)

	so0Restored := state.getStateObject(stateobjaddr0)
	// Update lazily-loaded values before comparing.
	key, _, _ := getKeyValue(stateobjaddr0, storageaddr.Bytes(), nil)
	so0Restored.GetState(state.db, key)
	so0Restored.Code(state.db)
	// non-deleted is equal (restored)
	compareStateObjects(so0Restored, so0, t)

	// deleted should be nil, both before and after restore of state copy
	so1Restored := state.getStateObject(stateobjaddr1)
	if so1Restored != nil {
		t.Fatalf("deleted object not nil after restoring snapshot: %+v", so1Restored)
	}
}

func compareStateObjects(so0, so1 *stateObject, t *testing.T) {
	if so0.Address() != so1.Address() {
		t.Fatalf("Address mismatch: have %v, want %v", so0.address, so1.address)
	}
	if so0.Balance().Cmp(so1.Balance()) != 0 {
		t.Fatalf("Balance mismatch: have %v, want %v", so0.Balance(), so1.Balance())
	}
	if so0.Nonce() != so1.Nonce() {
		t.Fatalf("Nonce mismatch: have %v, want %v", so0.Nonce(), so1.Nonce())
	}
	if so0.data.Root != so1.data.Root {
		t.Errorf("Root mismatch: have %x, want %x", so0.data.Root[:], so1.data.Root[:])
	}
	if !bytes.Equal(so0.CodeHash(), so1.CodeHash()) {
		t.Fatalf("CodeHash mismatch: have %v, want %v", so0.CodeHash(), so1.CodeHash())
	}
	if !bytes.Equal(so0.code, so1.code) {
		t.Fatalf("Code mismatch: have %v, want %v", so0.code, so1.code)
	}

	if len(so1.dirtyStorage) != len(so0.dirtyStorage) {
		t.Errorf("Dirty storage size mismatch: have %d, want %d", len(so1.dirtyStorage), len(so0.dirtyStorage))
	}
	for k, v := range so1.dirtyStorage {
		if so0.dirtyStorage[k] != v {
			t.Errorf("Dirty storage key %x mismatch: have %v, want %v", k, so0.dirtyStorage[k], v)
		}
	}
	for k, v := range so0.dirtyStorage {
		if so1.dirtyStorage[k] != v {
			t.Errorf("Dirty storage key %x mismatch: have %v, want none.", k, v)
		}
	}
	if len(so1.originStorage) != len(so0.originStorage) {
		t.Errorf("Origin storage size mismatch: have %d, want %d", len(so1.originStorage), len(so0.originStorage))
	}
	for k, v := range so1.originStorage {
		if so0.originStorage[k] != v {
			t.Errorf("Origin storage key %x mismatch: have %v, want %v", k, so0.originStorage[k], v)
		}
	}
	for k, v := range so0.originStorage {
		if so1.originStorage[k] != v {
			t.Errorf("Origin storage key %x mismatch: have %v, want none.", k, v)
		}
	}
}

func TestEmptyByte(t *testing.T) {
	db, _ := ethdb.NewLDBDatabase("D:\\resource\\venachain\\venachain-go\\data1", 0, 0)
	state, _ := New(common.Hash{}, NewDatabase(db))

	address := common.HexToAddress("0x823140710bf13990e4500136726d8b55")
	state.CreateAccount(address)
	so := state.getStateObject(address)

	//value := common.FromHex("0x823140710bf13990e4500136726d8b55")
	pvalue := []byte{'a'}
	key := []byte{'a'}

	//s.state.SetState(address, common.Hash{}, value)
	state.SetState(address, key, pvalue)
	state.Commit(false)

	if value := state.GetState(address, key); !bytes.Equal(value, pvalue) {
		t.Errorf("expected empty current value, got %x", value)
	}
	if value := state.GetCommittedState(address, key); !bytes.Equal(value, pvalue) {
		t.Errorf("expected empty committed value, got %x", value)
	}

	state.trie.NodeIterator(nil)
	it := trie.NewIterator(so.trie.NodeIterator(nil))
	for it.Next() {
		fmt.Println(it.Key, it.Value)
	}

	pvalue = []byte{}
	state.SetState(address, key, pvalue)
	state.Commit(false)

	if value := state.GetState(address, key); !bytes.Equal(value, pvalue) {
		t.Errorf("expected empty current value, got %x", value)
	}
	if value := state.GetCommittedState(address, key); !bytes.Equal(value, pvalue) {
		t.Errorf("expected empty committed value, got %x", value)
	}

	state.trie.NodeIterator(nil)
	it = trie.NewIterator(so.trie.NodeIterator(nil))
	for it.Next() {
		fmt.Println(it.Key, it.Value)
	}

	pvalue = []byte("bbb")
	state.SetState(address, key, pvalue)
	state.Commit(false)
	state.trie.NodeIterator(nil)
	it = trie.NewIterator(so.trie.NodeIterator(nil))
	for it.Next() {
		fmt.Println(it.Key, it.Value)
		fmt.Println(so.db.trie.GetKey(it.Value))
	}

}
