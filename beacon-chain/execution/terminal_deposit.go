package execution

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func (s *Service) currentDepositCount(ctx context.Context) ([]byte, error) {
	if s.cfg.terminalDepositContract == nil {
		return s.depositContractCaller.GetDepositCount(&bind.CallOpts{})
	}
	count, err := s.terminalDepositCount(ctx)
	if err != nil {
		return nil, err
	}
	return terminalDepositCountBytes(count), nil
}

const eip1967ImplementationSlot = "0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc"

var depositEventTopic = crypto.Keccak256Hash([]byte("DepositEvent(bytes,bytes,bytes,bytes,bytes)"))
var upgradedEventTopic = common.HexToHash("0xbc7cd75a20ee27fd9adebab32041f755214dbc6bffa90cc0225b39da2e5c2d3b")

// TerminalDepositContractConfig is an explicit proof contract for a network
// whose deposit proxy can no longer answer get_deposit_count().
type TerminalDepositContractConfig struct {
	ChainID                       uint64         `json:"chain_id"`
	Proxy                         common.Address `json:"proxy"`
	ProxyCodeHash                 common.Hash    `json:"proxy_code_hash"`
	TerminalBlock                 uint64         `json:"terminal_block"`
	TerminalBlockHash             common.Hash    `json:"terminal_block_hash"`
	TerminalTransaction           common.Hash    `json:"terminal_transaction"`
	TerminalSender                common.Address `json:"terminal_sender"`
	TerminalInput                 hexutil.Bytes  `json:"terminal_input"`
	CurrentImplementation         common.Address `json:"current_implementation"`
	CurrentImplementationCodeHash common.Hash    `json:"current_implementation_code_hash"`
	DepositCount                  uint64         `json:"deposit_count"`
	DepositRoot                   common.Hash    `json:"deposit_root"`
}

func ParseTerminalDepositContractConfig(raw string) (*TerminalDepositContractConfig, error) {
	if raw == "" {
		return nil, nil
	}
	var cfg TerminalDepositContractConfig
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("terminal deposit config must contain exactly one JSON object")
	}
	if cfg.ChainID == 0 || cfg.Proxy == (common.Address{}) || cfg.ProxyCodeHash == (common.Hash{}) || cfg.TerminalBlock == 0 ||
		cfg.TerminalBlockHash == (common.Hash{}) || cfg.TerminalTransaction == (common.Hash{}) || cfg.TerminalSender == (common.Address{}) || len(cfg.TerminalInput) == 0 ||
		cfg.CurrentImplementation == (common.Address{}) || cfg.CurrentImplementationCodeHash == (common.Hash{}) || cfg.DepositCount == 0 || cfg.DepositRoot == (common.Hash{}) {
		return nil, fmt.Errorf("all terminal deposit proof fields must be non-zero")
	}
	return &cfg, nil
}

type terminalBlockProof struct {
	Hash         common.Hash    `json:"hash"`
	Number       hexutil.Uint64 `json:"number"`
	Transactions []common.Hash  `json:"transactions"`
}

type terminalReceiptProof struct {
	BlockHash       common.Hash    `json:"blockHash"`
	BlockNumber     hexutil.Uint64 `json:"blockNumber"`
	TransactionHash common.Hash    `json:"transactionHash"`
	Status          hexutil.Uint64 `json:"status"`
	Logs            []types.Log    `json:"logs"`
}

type terminalTransactionProof struct {
	Hash        common.Hash     `json:"hash"`
	From        common.Address  `json:"from"`
	To          *common.Address `json:"to"`
	Input       hexutil.Bytes   `json:"input"`
	BlockHash   common.Hash     `json:"blockHash"`
	BlockNumber hexutil.Uint64  `json:"blockNumber"`
}

func (s *Service) terminalDepositCount(ctx context.Context) (uint64, error) {
	cfg := s.cfg.terminalDepositContract
	if cfg == nil {
		return 0, fmt.Errorf("terminal deposit mode is disabled")
	}
	if cfg.Proxy != s.cfg.depositContractAddr {
		return 0, fmt.Errorf("terminal deposit proxy does not match configured deposit contract")
	}
	var chainID hexutil.Uint64
	if err := s.rpcClient.CallContext(ctx, &chainID, "eth_chainId"); err != nil {
		return 0, fmt.Errorf("chain ID proof failed: %w", err)
	}
	if uint64(chainID) != cfg.ChainID {
		return 0, fmt.Errorf("chain ID proof failed: got %d", chainID)
	}
	blockTag := hexutil.EncodeUint64(cfg.TerminalBlock)
	var block terminalBlockProof
	if err := s.rpcClient.CallContext(ctx, &block, "eth_getBlockByNumber", blockTag, false); err != nil {
		return 0, fmt.Errorf("terminal block proof failed: %w", err)
	}
	if uint64(block.Number) != cfg.TerminalBlock || block.Hash != cfg.TerminalBlockHash {
		return 0, fmt.Errorf("terminal block is not canonical")
	}
	foundTx := false
	for _, tx := range block.Transactions {
		foundTx = foundTx || tx == cfg.TerminalTransaction
	}
	if !foundTx {
		return 0, fmt.Errorf("terminal transaction is absent from terminal block")
	}
	var transaction terminalTransactionProof
	if err := s.rpcClient.CallContext(ctx, &transaction, "eth_getTransactionByHash", cfg.TerminalTransaction); err != nil {
		return 0, fmt.Errorf("terminal transaction proof failed: %w", err)
	}
	if transaction.Hash != cfg.TerminalTransaction || transaction.From != cfg.TerminalSender || transaction.To == nil || *transaction.To != cfg.Proxy || !bytes.Equal(transaction.Input, cfg.TerminalInput) || transaction.BlockHash != cfg.TerminalBlockHash || uint64(transaction.BlockNumber) != cfg.TerminalBlock {
		return 0, fmt.Errorf("terminal transaction sender/destination/input proof failed")
	}
	var receipt terminalReceiptProof
	if err := s.rpcClient.CallContext(ctx, &receipt, "eth_getTransactionReceipt", cfg.TerminalTransaction); err != nil {
		return 0, fmt.Errorf("terminal receipt proof failed: %w", err)
	}
	if receipt.Status != 1 || receipt.BlockHash != cfg.TerminalBlockHash || uint64(receipt.BlockNumber) != cfg.TerminalBlock || receipt.TransactionHash != cfg.TerminalTransaction {
		return 0, fmt.Errorf("terminal transaction receipt proof failed")
	}
	wantImplementationTopic := common.BytesToHash(cfg.CurrentImplementation.Bytes())
	foundUpgrade := false
	for _, event := range receipt.Logs {
		foundUpgrade = foundUpgrade || (event.Address == cfg.Proxy && event.BlockHash == cfg.TerminalBlockHash &&
			event.TxHash == cfg.TerminalTransaction && len(event.Topics) == 2 && event.Topics[0] == upgradedEventTopic && event.Topics[1] == wantImplementationTopic)
	}
	if !foundUpgrade {
		return 0, fmt.Errorf("terminal receipt lacks exact Upgraded event proof")
	}
	var latest terminalBlockProof
	if err := s.rpcClient.CallContext(ctx, &latest, "eth_getBlockByNumber", "latest", false); err != nil {
		return 0, fmt.Errorf("current canonical head proof failed: %w", err)
	}
	if uint64(latest.Number) <= cfg.TerminalBlock {
		return 0, fmt.Errorf("canonical head does not descend beyond terminal block")
	}
	var code hexutil.Bytes
	if err := s.rpcClient.CallContext(ctx, &code, "eth_getCode", cfg.Proxy, blockTag); err != nil {
		return 0, fmt.Errorf("proxy code proof failed: %w", err)
	}
	if crypto.Keccak256Hash(code) != cfg.ProxyCodeHash {
		return 0, fmt.Errorf("proxy code hash mismatch")
	}
	var implementation common.Hash
	if err := s.rpcClient.CallContext(ctx, &implementation, "eth_getStorageAt", cfg.Proxy, eip1967ImplementationSlot, "latest"); err != nil {
		return 0, fmt.Errorf("proxy implementation proof failed: %w", err)
	}
	if common.BytesToAddress(implementation.Bytes()[12:]) != cfg.CurrentImplementation {
		return 0, fmt.Errorf("proxy implementation mismatch")
	}
	var implementationCode hexutil.Bytes
	if err := s.rpcClient.CallContext(ctx, &implementationCode, "eth_getCode", cfg.CurrentImplementation, "latest"); err != nil {
		return 0, fmt.Errorf("current implementation code proof failed: %w", err)
	}
	if crypto.Keccak256Hash(implementationCode) != cfg.CurrentImplementationCodeHash {
		return 0, fmt.Errorf("current implementation code hash mismatch")
	}
	fState := s.cfg.finalizedStateAtStartup
	if fState == nil || fState.IsNil() || fState.Eth1DepositIndex() != cfg.DepositCount ||
		fState.Eth1Data().DepositCount != cfg.DepositCount || common.BytesToHash(fState.Eth1Data().DepositRoot) != cfg.DepositRoot {
		return 0, fmt.Errorf("finalized beacon deposit state proof failed")
	}
	if err := s.verifyTerminalDepositCaches(ctx, cfg); err != nil {
		return 0, err
	}
	for from := cfg.TerminalBlock + 1; from <= uint64(latest.Number); from += 10000 {
		to := min(from+9999, uint64(latest.Number))
		var logs []types.Log
		filter := map[string]interface{}{"fromBlock": hexutil.EncodeUint64(from), "toBlock": hexutil.EncodeUint64(to), "address": cfg.Proxy, "topics": []interface{}{depositEventTopic}}
		if err := s.rpcClient.CallContext(ctx, &logs, "eth_getLogs", filter); err != nil {
			return 0, fmt.Errorf("post-terminal deposit log proof failed: %w", err)
		}
		if len(logs) != 0 {
			return 0, fmt.Errorf("post-terminal DepositEvent mutation detected")
		}
	}
	return cfg.DepositCount, nil
}

func (s *Service) verifyTerminalDepositCaches(ctx context.Context, cfg *TerminalDepositContractConfig) error {
	if cfg.DepositCount == 0 || uint64(s.depositTrie.NumOfItems()) != cfg.DepositCount ||
		s.lastReceivedMerkleIndex != int64(cfg.DepositCount)-1 {
		return fmt.Errorf("cached deposit trie count/index proof failed")
	}
	root, err := s.depositTrie.HashTreeRoot()
	if err != nil {
		return fmt.Errorf("cached deposit trie root proof failed: %w", err)
	}
	if common.Hash(root) != cfg.DepositRoot {
		return fmt.Errorf("cached deposit trie root mismatch")
	}
	containers := s.cfg.depositCache.AllDepositContainers(ctx)
	if uint64(len(containers)) != cfg.DepositCount || len(containers) == 0 || containers[len(containers)-1].Index != int64(cfg.DepositCount)-1 {
		return fmt.Errorf("cached deposit containers count/index proof failed")
	}
	for i, container := range containers {
		if container == nil || container.Index != int64(i) {
			return fmt.Errorf("cached deposit containers are not contiguous")
		}
	}
	if common.BytesToHash(containers[len(containers)-1].DepositRoot) != cfg.DepositRoot {
		return fmt.Errorf("cached deposit container root mismatch")
	}
	return nil
}

func terminalDepositCountBytes(count uint64) []byte {
	raw := make([]byte, 8)
	binary.LittleEndian.PutUint64(raw, count)
	return raw
}
