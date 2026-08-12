package execution

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/prysmaticlabs/prysm/v5/beacon-chain/cache/depositsnapshot"
	"github.com/prysmaticlabs/prysm/v5/network"
	ethpb "github.com/prysmaticlabs/prysm/v5/proto/prysm/v1alpha1"
	"github.com/prysmaticlabs/prysm/v5/testing/assert"
	"github.com/prysmaticlabs/prysm/v5/testing/require"
	"github.com/prysmaticlabs/prysm/v5/testing/util"
)

const validTerminalConfig = `{
  "chain_id":1480,
  "proxy":"0x17BbE91c315Bf14f38F6D35052a827cadfFe184e",
  "proxy_code_hash":"0x4a8eea8d15ed68daa5daffbeed273342c8ed742e468b1fca935b75793c458f4d",
  "terminal_block":9475269,
  "terminal_block_hash":"0xd50f2e32c4968e15dc0986fca89d663633c17fd986051647290b6d6d454b77f9",
  "terminal_transaction":"0x95e9c26e01a35ab939b3442a89048b356f887d132b7afa13550e41a61cb86eb5",
  "terminal_sender":"0x2AC93684679a5bdA03C6160def908CdB8D46792f",
  "terminal_input":"0x4f1ef286000000000000000000000000abc8637b553635654539b19d6cb53d64d4274325000000000000000000000000000000000000000000000000000000000000004000000000000000000000000000000000000000000000000000000000000000043ccfd60b00000000000000000000000000000000000000000000000000000000",
  "current_implementation":"0xabc8637b553635654539b19d6cb53d64d4274325",
  "current_implementation_code_hash":"0x3734609c2de1bd57a2d3b63ffb10892e129edc5c350165b64f593db5c68b9c7b",
  "deposit_count":32,
  "deposit_root":"0x60dff8f6a92d68799e7653acdcefa253a1c2603b4dd071d8265491b465172401"
}`

func TestParseTerminalDepositContractConfig(t *testing.T) {
	cfg, err := ParseTerminalDepositContractConfig("")
	require.NoError(t, err)
	assert.Equal(t, (*TerminalDepositContractConfig)(nil), cfg)

	cfg, err = ParseTerminalDepositContractConfig(validTerminalConfig)
	require.NoError(t, err)
	assert.Equal(t, uint64(1480), cfg.ChainID)
	assert.Equal(t, uint64(32), cfg.DepositCount)

	for _, mutation := range []string{
		strings.Replace(validTerminalConfig, `"chain_id":1480`, `"chain_id":0`, 1),
		strings.Replace(validTerminalConfig, `"deposit_count":32`, `"deposit_count":0`, 1),
		strings.Replace(validTerminalConfig, `"chain_id":1480`, `"unknown":1,"chain_id":1480`, 1),
		validTerminalConfig + `{}`,
		`not-json`,
	} {
		_, err := ParseTerminalDepositContractConfig(mutation)
		assert.NotNil(t, err)
	}
}

func TestTerminalDepositCountBytesIsLittleEndianUint64(t *testing.T) {
	raw := terminalDepositCountBytes(32)
	assert.Equal(t, 8, len(raw))
	assert.Equal(t, uint64(32), binary.LittleEndian.Uint64(raw))
}

func TestProcessDepositLogRejectsPostTerminalMutationBeforeParsing(t *testing.T) {
	cfg, err := ParseTerminalDepositContractConfig(validTerminalConfig)
	require.NoError(t, err)
	s := &Service{cfg: &config{terminalDepositContract: cfg}}
	err = s.ProcessDepositLog(context.Background(), &gethtypes.Log{BlockNumber: cfg.TerminalBlock + 1})
	assert.ErrorContains(t, "after terminal deposit block", err)
}

func TestProcessLogKeepsIgnoringPostTerminalNonDepositEvents(t *testing.T) {
	cfg, err := ParseTerminalDepositContractConfig(validTerminalConfig)
	require.NoError(t, err)
	s := &Service{cfg: &config{terminalDepositContract: cfg}}
	require.NoError(t, s.ProcessLog(context.Background(), &gethtypes.Log{BlockNumber: cfg.TerminalBlock + 1, Topics: []common.Hash{{9}}}))
}

func TestVerifyTerminalDepositCachesGuardsCountUnderflow(t *testing.T) {
	cache, err := depositsnapshot.New()
	require.NoError(t, err)
	s := &Service{cfg: &config{depositCache: cache}, depositTrie: depositsnapshot.NewDepositTree(), lastReceivedMerkleIndex: -1}
	err = s.verifyTerminalDepositCaches(context.Background(), &TerminalDepositContractConfig{DepositCount: 0, DepositRoot: common.Hash{1}}, false)
	assert.ErrorContains(t, "count/index", err)
}

type fakeDepositCaller struct{ calls int }

func (f *fakeDepositCaller) GetDepositCount(*bind.CallOpts) ([]byte, error) {
	f.calls++
	return terminalDepositCountBytes(7), nil
}

type terminalRPCFake struct {
	cfg                *TerminalDepositContractConfig
	code               []byte
	implementationCode []byte
	codeCalls          int
	failure            string
	nonzeroLog         bool
	blockCalls         int
	codeBlockTags      []string
}

func (f *terminalRPCFake) Close()                          {}
func (f *terminalRPCFake) BatchCall([]rpc.BatchElem) error { return nil }
func (f *terminalRPCFake) CallContext(_ context.Context, result interface{}, method string, args ...interface{}) error {
	if f.failure == method+":error" {
		return errors.New("injected RPC failure")
	}
	switch method {
	case "eth_chainId":
		v := hexutil.Uint64(f.cfg.ChainID)
		if f.failure == "chain-id" {
			v++
		}
		*(result.(*hexutil.Uint64)) = v
	case "eth_getBlockByNumber":
		p := result.(*terminalBlockProof)
		f.blockCalls++
		if f.blockCalls == 1 {
			*p = terminalBlockProof{Hash: f.cfg.TerminalBlockHash, Number: hexutil.Uint64(f.cfg.TerminalBlock), Transactions: []common.Hash{f.cfg.TerminalTransaction}}
			if f.failure == "block-hash" {
				p.Hash = common.Hash{9}
			}
			if f.failure == "block-number" {
				p.Number--
			}
			if f.failure == "tx-absent" {
				p.Transactions = nil
			}
		} else {
			*p = terminalBlockProof{Hash: common.Hash{3}, Number: hexutil.Uint64(f.cfg.TerminalBlock + 100)}
			if f.failure == "latest" {
				p.Number = hexutil.Uint64(f.cfg.TerminalBlock)
			}
			if f.failure == "latest:error" {
				return errors.New("latest failed")
			}
		}
	case "eth_getTransactionReceipt":
		r := result.(*terminalReceiptProof)
		*r = terminalReceiptProof{BlockHash: f.cfg.TerminalBlockHash, BlockNumber: hexutil.Uint64(f.cfg.TerminalBlock), TransactionHash: f.cfg.TerminalTransaction, Status: 1, Logs: []gethtypes.Log{{Address: f.cfg.Proxy, BlockHash: f.cfg.TerminalBlockHash, TxHash: f.cfg.TerminalTransaction, Topics: []common.Hash{upgradedEventTopic, common.BytesToHash(f.cfg.CurrentImplementation.Bytes())}}}}
		if f.failure == "receipt-status" {
			r.Status = 0
		}
		if f.failure == "receipt-block" {
			r.BlockHash = common.Hash{8}
		}
		if f.failure == "receipt-number" {
			r.BlockNumber--
		}
		if f.failure == "receipt-tx" {
			r.TransactionHash = common.Hash{6}
		}
		if f.failure == "upgrade-log-address" {
			r.Logs[0].Address = common.Address{6}
		}
		if f.failure == "upgrade-log-topic" {
			r.Logs[0].Topics[0] = common.Hash{6}
		}
		if f.failure == "upgrade-log-impl" {
			r.Logs[0].Topics[1] = common.Hash{6}
		}
		if f.failure == "upgrade-log-block" {
			r.Logs[0].BlockHash = common.Hash{6}
		}
		if f.failure == "upgrade-log-tx" {
			r.Logs[0].TxHash = common.Hash{6}
		}
	case "eth_getTransactionByHash":
		tx := result.(*terminalTransactionProof)
		to := f.cfg.Proxy
		*tx = terminalTransactionProof{Hash: f.cfg.TerminalTransaction, From: f.cfg.TerminalSender, To: &to, Input: append([]byte(nil), f.cfg.TerminalInput...), BlockHash: f.cfg.TerminalBlockHash, BlockNumber: hexutil.Uint64(f.cfg.TerminalBlock)}
		if f.failure == "tx-hash" {
			tx.Hash = common.Hash{5}
		}
		if f.failure == "tx-from" {
			tx.From = common.Address{5}
		}
		if f.failure == "tx-to" {
			wrong := common.Address{5}
			tx.To = &wrong
		}
		if f.failure == "tx-input" {
			tx.Input = []byte{5}
		}
		if f.failure == "tx-block-hash" {
			tx.BlockHash = common.Hash{5}
		}
		if f.failure == "tx-block-number" {
			tx.BlockNumber--
		}
	case "eth_getCode":
		f.codeBlockTags = append(f.codeBlockTags, args[1].(string))
		f.codeCalls++
		if f.codeCalls == 1 {
			*(result.(*hexutil.Bytes)) = f.code
			if f.failure == "code-hash" {
				*(result.(*hexutil.Bytes)) = []byte{0xff}
			}
		} else {
			*(result.(*hexutil.Bytes)) = f.implementationCode
			if f.failure == "implementation-code-hash" {
				*(result.(*hexutil.Bytes)) = []byte{0xee}
			}
		}
	case "eth_getStorageAt":
		*(result.(*common.Hash)) = common.BytesToHash(f.cfg.CurrentImplementation.Bytes())
		if f.failure == "implementation" {
			*(result.(*common.Hash)) = common.Hash{7}
		}
	case "eth_getLogs":
		if f.nonzeroLog {
			*(result.(*[]gethtypes.Log)) = []gethtypes.Log{{BlockNumber: f.cfg.TerminalBlock + 1}}
		}
	}
	return nil
}

func terminalServiceFixture(t *testing.T) (*Service, *terminalRPCFake, *fakeDepositCaller) {
	count := 4
	deps, _, err := util.DeterministicDepositsAndKeys(uint64(count))
	require.NoError(t, err)
	tree := depositsnapshot.NewDepositTree()
	cache, err := depositsnapshot.New()
	require.NoError(t, err)
	containers := make([]*ethpb.DepositContainer, count)
	var root [32]byte
	for i, dep := range deps {
		item, err := dep.Data.HashTreeRoot()
		require.NoError(t, err)
		require.NoError(t, tree.Insert(item[:], i))
		root, err = tree.HashTreeRoot()
		require.NoError(t, err)
		containers[i] = &ethpb.DepositContainer{Index: int64(i), Deposit: dep, DepositRoot: append([]byte(nil), root[:]...)}
	}
	cache.InsertDepositContainers(context.Background(), containers)
	st, err := util.NewBeaconState()
	require.NoError(t, err)
	require.NoError(t, st.SetEth1DepositIndex(uint64(count)))
	require.NoError(t, st.SetEth1Data(&ethpb.Eth1Data{DepositCount: uint64(count), DepositRoot: root[:]}))
	cfg := &TerminalDepositContractConfig{ChainID: 1480, Proxy: common.HexToAddress("0x1234"), TerminalBlock: 100, TerminalBlockHash: common.Hash{1}, TerminalTransaction: common.Hash{2}, TerminalSender: common.HexToAddress("0xabcd"), TerminalInput: []byte{1, 2}, CurrentImplementation: common.HexToAddress("0x5678"), DepositCount: uint64(count), DepositRoot: common.Hash(root)}
	code := []byte{1, 2, 3}
	cfg.ProxyCodeHash = crypto.Keccak256Hash(code)
	implementationCode := []byte{4, 5, 6}
	cfg.CurrentImplementationCodeHash = crypto.Keccak256Hash(implementationCode)
	rpcFake := &terminalRPCFake{cfg: cfg, code: code, implementationCode: implementationCode}
	caller := &fakeDepositCaller{}
	s := &Service{cfg: &config{depositContractAddr: cfg.Proxy, terminalDepositContract: cfg, finalizedStateAtStartup: st, depositCache: cache, currHttpEndpoint: network.Endpoint{}}, rpcClient: rpcFake, depositContractCaller: caller, depositTrie: tree, lastReceivedMerkleIndex: int64(count - 1)}
	return s, rpcFake, caller
}

func TestCurrentDepositCountDefaultCallsContract(t *testing.T) {
	caller := &fakeDepositCaller{}
	s := &Service{cfg: &config{}, depositContractCaller: caller}
	raw, err := s.currentDepositCount(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(7), binary.LittleEndian.Uint64(raw))
	assert.Equal(t, 1, caller.calls)
}

func TestCurrentDepositCountTerminalHappyPathNeverCallsContract(t *testing.T) {
	s, rpcFake, caller := terminalServiceFixture(t)
	raw, err := s.currentDepositCount(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(4), binary.LittleEndian.Uint64(raw))
	assert.Equal(t, 0, caller.calls)
	assert.DeepEqual(t, []string{"latest", "latest"}, rpcFake.codeBlockTags)
}

func TestTerminalDepositProofDoesNotRequireHistoricalState(t *testing.T) {
	s, rpcFake, _ := terminalServiceFixture(t)
	_, err := s.currentDepositCount(context.Background())
	require.NoError(t, err)
	for _, tag := range rpcFake.codeBlockTags {
		assert.Equal(t, "latest", tag)
	}
}

func TestTerminalDepositRPCProofFailures(t *testing.T) {
	for _, failure := range []string{"chain-id", "eth_chainId:error", "eth_getBlockByNumber:error", "block-hash", "block-number", "tx-absent", "eth_getTransactionByHash:error", "tx-hash", "tx-from", "tx-to", "tx-input", "tx-block-hash", "tx-block-number", "eth_getTransactionReceipt:error", "receipt-status", "receipt-block", "receipt-number", "receipt-tx", "upgrade-log-address", "upgrade-log-topic", "upgrade-log-impl", "upgrade-log-block", "upgrade-log-tx", "latest", "latest:error", "eth_getCode:error", "code-hash", "eth_getStorageAt:error", "implementation", "implementation-code-hash", "eth_getLogs:error"} {
		t.Run(failure, func(t *testing.T) {
			s, f, _ := terminalServiceFixture(t)
			f.failure = failure
			_, err := s.currentDepositCount(context.Background())
			assert.NotNil(t, err)
		})
	}
	t.Run("post-terminal-log", func(t *testing.T) {
		s, f, _ := terminalServiceFixture(t)
		f.nonzeroLog = true
		_, err := s.currentDepositCount(context.Background())
		assert.NotNil(t, err)
	})
}

func TestTerminalDepositProxyMustMatchConfiguredDepositContract(t *testing.T) {
	s, _, _ := terminalServiceFixture(t)
	s.cfg.depositContractAddr = common.HexToAddress("0xdead")
	_, err := s.currentDepositCount(context.Background())
	assert.ErrorContains(t, "does not match", err)
}

func TestTerminalDepositLocalProofFailures(t *testing.T) {
	t.Run("finalized-nil", func(t *testing.T) {
		s, _, _ := terminalServiceFixture(t)
		s.cfg.finalizedStateAtStartup = nil
		_, err := s.currentDepositCount(context.Background())
		assert.NotNil(t, err)
	})
	t.Run("finalized-index", func(t *testing.T) {
		s, _, _ := terminalServiceFixture(t)
		require.NoError(t, s.cfg.finalizedStateAtStartup.SetEth1DepositIndex(3))
		_, err := s.currentDepositCount(context.Background())
		assert.NotNil(t, err)
	})
	t.Run("finalized-count", func(t *testing.T) {
		s, _, _ := terminalServiceFixture(t)
		data := s.cfg.finalizedStateAtStartup.Eth1Data()
		data.DepositCount = 3
		require.NoError(t, s.cfg.finalizedStateAtStartup.SetEth1Data(data))
		_, err := s.currentDepositCount(context.Background())
		assert.NotNil(t, err)
	})
	t.Run("finalized-root", func(t *testing.T) {
		s, _, _ := terminalServiceFixture(t)
		data := s.cfg.finalizedStateAtStartup.Eth1Data()
		data.DepositRoot = common.Hash{9}.Bytes()
		require.NoError(t, s.cfg.finalizedStateAtStartup.SetEth1Data(data))
		_, err := s.currentDepositCount(context.Background())
		assert.NotNil(t, err)
	})
	t.Run("trie-index", func(t *testing.T) {
		s, _, _ := terminalServiceFixture(t)
		s.lastReceivedMerkleIndex--
		_, err := s.currentDepositCount(context.Background())
		assert.NotNil(t, err)
	})
	t.Run("trie-root", func(t *testing.T) {
		s, _, _ := terminalServiceFixture(t)
		s.cfg.terminalDepositContract.DepositRoot = common.Hash{9}
		err := s.verifyTerminalDepositCaches(context.Background(), s.cfg.terminalDepositContract, true)
		assert.NotNil(t, err)
	})
	t.Run("containers", func(t *testing.T) {
		s, _, _ := terminalServiceFixture(t)
		empty, err := depositsnapshot.New()
		require.NoError(t, err)
		s.cfg.depositCache = empty
		_, err = s.currentDepositCount(context.Background())
		assert.NotNil(t, err)
	})
}

func TestTerminalDepositCacheReplayBoundaries(t *testing.T) {
	t.Run("exact", func(t *testing.T) {
		s, _, _ := terminalServiceFixture(t)
		require.NoError(t, s.verifyTerminalDepositCaches(context.Background(), s.cfg.terminalDepositContract, false))
		require.NoError(t, s.verifyTerminalDepositCaches(context.Background(), s.cfg.terminalDepositContract, true))
	})
	t.Run("empty-is-valid-before-replay-only", func(t *testing.T) {
		s, _, _ := terminalServiceFixture(t)
		tree := depositsnapshot.NewDepositTree()
		cache, err := depositsnapshot.New()
		require.NoError(t, err)
		s.depositTrie = tree
		s.cfg.depositCache = cache
		s.lastReceivedMerkleIndex = -1
		require.NoError(t, s.verifyTerminalDepositCaches(context.Background(), s.cfg.terminalDepositContract, false))
		err = s.verifyTerminalDepositCaches(context.Background(), s.cfg.terminalDepositContract, true)
		assert.ErrorContains(t, "incomplete", err)
	})
	t.Run("partial-is-valid-before-replay-only", func(t *testing.T) {
		s, _, _ := terminalServiceFixture(t)
		original := s.cfg.depositCache.AllDepositContainers(context.Background())
		tree := depositsnapshot.NewDepositTree()
		cache, err := depositsnapshot.New()
		require.NoError(t, err)
		for i := 0; i < 2; i++ {
			root, err := original[i].Deposit.Data.HashTreeRoot()
			require.NoError(t, err)
			require.NoError(t, tree.Insert(root[:], i))
		}
		partialRoot, err := tree.HashTreeRoot()
		require.NoError(t, err)
		partial := original[:2]
		partial[1].DepositRoot = partialRoot[:]
		cache.InsertDepositContainers(context.Background(), partial)
		s.depositTrie = tree
		s.cfg.depositCache = cache
		s.lastReceivedMerkleIndex = 1
		require.NoError(t, s.verifyTerminalDepositCaches(context.Background(), s.cfg.terminalDepositContract, false))
		err = s.verifyTerminalDepositCaches(context.Background(), s.cfg.terminalDepositContract, true)
		assert.ErrorContains(t, "incomplete", err)
	})
	t.Run("ahead", func(t *testing.T) {
		s, _, _ := terminalServiceFixture(t)
		s.cfg.terminalDepositContract.DepositCount = 3
		err := s.verifyTerminalDepositCaches(context.Background(), s.cfg.terminalDepositContract, false)
		assert.ErrorContains(t, "count/index", err)
	})
	t.Run("noncontiguous", func(t *testing.T) {
		s, _, _ := terminalServiceFixture(t)
		ctrs := s.cfg.depositCache.AllDepositContainers(context.Background())
		ctrs[2].Index = 9
		err := s.verifyTerminalDepositCaches(context.Background(), s.cfg.terminalDepositContract, false)
		assert.ErrorContains(t, "not contiguous", err)
	})
	t.Run("wrong-root-after-replay", func(t *testing.T) {
		s, _, _ := terminalServiceFixture(t)
		s.cfg.terminalDepositContract.DepositRoot = common.Hash{9}
		err := s.verifyTerminalDepositCaches(context.Background(), s.cfg.terminalDepositContract, true)
		assert.ErrorContains(t, "trie root mismatch", err)
	})
}
