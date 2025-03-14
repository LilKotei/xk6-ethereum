// xk6 build --with github.com/distribworks/xk6-ethereum=.
package ethereum

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"sync"
	"time"

	"github.com/grafana/sobek"
	"github.com/umbracle/ethgo"
	"github.com/umbracle/ethgo/abi"
	"github.com/umbracle/ethgo/contract"
	"github.com/umbracle/ethgo/jsonrpc"
	"github.com/umbracle/ethgo/wallet"
	"go.k6.io/k6/js/modules"
	"go.k6.io/k6/metrics"
)

type Transaction struct {
	From      string
	To        string
	Input     []byte
	GasPrice  uint64
	GasFeeCap uint64
	GasTipCap uint64
	Gas       uint64
	Value     int64
	Nonce     uint64
	// eip-2930 values
	ChainId int64
}

type Client struct {
	w       *wallet.Key
	client  *jsonrpc.Client
	chainID *big.Int
	vu      modules.VU
	metrics ethMetrics
	opts    *options
}

// ---------------------------------------------------------------------
// ERC20 Write ABI (for mint and transfer)
// ---------------------------------------------------------------------
const erc20ABI = `[
	{"constant": false, "inputs": [{"name": "_to", "type": "address"},{"name": "_amount", "type": "uint256"}], "name": "mint", "outputs": [], "type": "function"},
	{"constant": false, "inputs": [{"name": "_to", "type": "address"},{"name": "_value", "type": "uint256"}], "name": "transfer", "outputs": [{"name": "","type": "bool"}], "type": "function"}
]`

// ---------------------------------------------------------------------
// ERC20 Read ABI (for balanceOf, totalSupply, allowance)
// ---------------------------------------------------------------------
const erc20ReadABI = `[
  {
    "inputs": [{"internalType": "address", "name": "_owner", "type": "address"}],
    "name": "balanceOf",
    "outputs": [{"internalType": "uint256", "name": "", "type": "uint256"}],
    "stateMutability": "view",
    "type": "function"
  },
  {
    "inputs": [],
    "name": "totalSupply",
    "outputs": [{"internalType": "uint256", "name": "", "type": "uint256"}],
    "stateMutability": "view",
    "type": "function"
  },
  {
    "inputs": [
      {"internalType": "address", "name": "_owner", "type": "address"},
      {"internalType": "address", "name": "_spender", "type": "address"}
    ],
    "name": "allowance",
    "outputs": [{"internalType": "uint256", "name": "", "type": "uint256"}],
    "stateMutability": "view",
    "type": "function"
  }
]`

// ---------------------------------------------------------------------
// Exported ERC20 utilities for k6 scripts.
// These functions do not affect the native functions.
// ---------------------------------------------------------------------
func (c *Client) Exports() modules.Exports {
	return modules.Exports{}
}

func (c *Client) Call(method string, params ...interface{}) (interface{}, error) {
	t := time.Now()
	var out interface{}
	err := c.client.Call(method, &out, params...)
	c.reportMetricsFromStats(method, time.Since(t))
	return out, err
}

func (c *Client) GasPrice() (uint64, error) {
	t := time.Now()
	g, err := c.client.Eth().GasPrice()
	c.reportMetricsFromStats("gas_price", time.Since(t))
	return g, err
}

func (c *Client) GetBalance(address string, blockNumber ethgo.BlockNumber) (uint64, error) {
	b, err := c.client.Eth().GetBalance(ethgo.HexToAddress(address), blockNumber)
	return b.Uint64(), err
}

func (c *Client) BlockNumber() (uint64, error) {
	return c.client.Eth().BlockNumber()
}

func (c *Client) GetBlockByNumber(number ethgo.BlockNumber, full bool) (*ethgo.Block, error) {
	return c.client.Eth().GetBlockByNumber(number, full)
}

func (c *Client) GetNonce(address string) (uint64, error) {
	return c.client.Eth().GetNonce(ethgo.HexToAddress(address), ethgo.Pending)
}

func (c *Client) EstimateGas(tx Transaction) (uint64, error) {
	to := ethgo.HexToAddress(tx.To)
	msg := &ethgo.CallMsg{
		From:     c.w.Address(),
		To:       &to,
		Value:    big.NewInt(tx.Value),
		Data:     tx.Input,
		GasPrice: tx.GasPrice,
	}
	gas, err := c.client.Eth().EstimateGas(msg)
	if err != nil {
		return 0, fmt.Errorf("failed to estimate gas: %w", err)
	}
	return gas, nil
}

func (c *Client) SendTransaction(tx Transaction) (string, error) {
	to := ethgo.HexToAddress(tx.To)
	if tx.Gas == 0 {
		tx.Gas = 21000
	}
	if tx.GasPrice == 0 && tx.GasFeeCap == 0 && tx.GasTipCap == 0 {
		tx.GasPrice = 5242880
	}
	t := &ethgo.Transaction{
		Type:     ethgo.TransactionLegacy,
		From:     ethgo.HexToAddress(tx.From),
		To:       &to,
		Value:    big.NewInt(tx.Value),
		Gas:      tx.Gas,
		GasPrice: tx.GasPrice,
	}
	if tx.GasFeeCap > 0 || tx.GasTipCap > 0 {
		t.Type = ethgo.TransactionDynamicFee
		t.GasPrice = 0
		t.MaxFeePerGas = big.NewInt(0).SetUint64(tx.GasFeeCap)
		t.MaxPriorityFeePerGas = big.NewInt(0).SetUint64(tx.GasTipCap)
	}
	h, err := c.client.Eth().SendTransaction(t)
	return h.String(), err
}

func (c *Client) SendRawTransaction(tx Transaction) (string, error) {
	to := ethgo.HexToAddress(tx.To)
	gas, err := c.EstimateGas(tx)
	if err != nil {
		return "", err
	}
	t := &ethgo.Transaction{
		Type:     ethgo.TransactionLegacy,
		From:     c.w.Address(),
		To:       &to,
		Value:    big.NewInt(tx.Value),
		Gas:      gas,
		GasPrice: tx.GasPrice,
		Nonce:    tx.Nonce,
		Input:    tx.Input,
		ChainID:  c.chainID,
	}
	if tx.GasFeeCap > 0 || tx.GasTipCap > 0 {
		t.Type = ethgo.TransactionDynamicFee
		t.GasPrice = 0
		t.MaxFeePerGas = big.NewInt(0).SetUint64(tx.GasFeeCap)
		t.MaxPriorityFeePerGas = big.NewInt(0).SetUint64(tx.GasTipCap)
	}
	s := wallet.NewEIP155Signer(t.ChainID.Uint64())
	st, err := s.SignTx(t, c.w)
	if err != nil {
		return "", err
	}
	trlp, err := st.MarshalRLPTo(nil)
	if err != nil {
		return "", fmt.Errorf("failed to marshal tx: %e", err)
	}
	h, err := c.client.Eth().SendRawTransaction(trlp)
	return h.String(), err
}

func (c *Client) GetTransactionReceipt(hash string) (*ethgo.Receipt, error) {
	r, err := c.client.Eth().GetTransactionReceipt(ethgo.HexToHash(hash))
	if err != nil {
		return nil, err
	}
	if r != nil {
		return r, nil
	}
	return nil, fmt.Errorf("not found")
}

func (c *Client) WaitForTransactionReceipt(hash string) *sobek.Promise {
	promise, resolve, reject := c.makeHandledPromise()
	now := time.Now()
	go func() {
		for {
			receipt, err := c.GetTransactionReceipt(hash)
			if err != nil {
				if err.Error() != "not found" {
					reject(err)
					return
				}
			}
			if receipt != nil {
				if c.vu != nil {
					metrics.PushIfNotDone(c.vu.Context(), c.vu.State().Samples, metrics.Sample{
						TimeSeries: metrics.TimeSeries{
							Metric: c.metrics.TimeToMine,
							Tags:   metrics.NewRegistry().RootTagSet(),
						},
						Value: float64(time.Since(now) / time.Millisecond),
						Time:  time.Now(),
					})
				}
				resolve(receipt)
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	return promise
}

func (c *Client) Accounts() ([]string, error) {
	accounts, err := c.client.Eth().Accounts()
	if err != nil {
		return nil, err
	}
	addresses := make([]string, len(accounts))
	for i, a := range accounts {
		addresses[i] = a.String()
	}
	return addresses, nil
}

func (c *Client) NewContract(address string, abistr string) (*Contract, error) {
	contractABI, err := abi.NewABI(abistr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse abi: %w", err)
	}
	opts := []contract.ContractOption{
		contract.WithJsonRPC(c.client.Eth()),
		contract.WithSender(c.w),
	}
	contract := contract.NewContract(ethgo.HexToAddress(address), contractABI, opts...)
	return &Contract{
		Contract: contract,
		client:   c,
	}, nil
}

func (c *Client) DeployContract(abistr string, bytecode string, args ...interface{}) (*ethgo.Receipt, error) {
	contractABI, err := abi.NewABI(abistr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse abi: %w", err)
	}
	contractBytecode, err := hex.DecodeString(bytecode)
	if err != nil {
		return nil, fmt.Errorf("failed to decode bytecode: %w", err)
	}
	opts := []contract.ContractOption{
		contract.WithJsonRPC(c.client.Eth()),
		contract.WithSender(c.w),
	}
	txn, err := contract.DeployContract(contractABI, contractBytecode, args, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to deploy contract: %w", err)
	}
	txn.WithOpts(&contract.TxnOpts{
		GasLimit: 1500000,
	})
	err = txn.Do()
	if err != nil {
		return nil, fmt.Errorf("failed to deploy contract: %w", err)
	}
	receipt, err := txn.Wait()
	if err != nil {
		return nil, fmt.Errorf("failed waiting to deploy contract: %w", err)
	}
	return receipt, nil
}

// ---------------------------------------------------------------------
// ERC20 Write Utility: Mint tokens
// ---------------------------------------------------------------------
func (c *Client) ERC20Mint(contractAddress, to string, amount *big.Int) (string, error) {
	parsedABI, err := abi.NewABI(erc20ABI)
	if err != nil {
		return "", fmt.Errorf("failed to parse ERC20 ABI: %w", err)
	}
	mintMethod, ok := parsedABI.Methods["mint"]
	if !ok {
		return "", fmt.Errorf("mint method not found in ABI")
	}
	data, err := mintMethod.Encode([]any{ethgo.HexToAddress(to), amount})
	if err != nil {
		return "", fmt.Errorf("failed to encode mint arguments: %w", err)
	}
	senderAddress := c.w.Address().String()
	nonce, err := c.GetNonce(senderAddress)
	if err != nil {
		return "", fmt.Errorf("failed to get nonce: %w", err)
	}
	gasPrice, err := c.GasPrice()
	if err != nil {
		return "", fmt.Errorf("failed to get gas price: %w", err)
	}
	tx := Transaction{
		From:     senderAddress,
		To:       contractAddress,
		Input:    data,
		Gas:      100000,
		GasPrice: gasPrice,
		Nonce:    nonce,
		ChainId:  c.chainID.Int64(),
	}
	if estimatedGas, err := c.EstimateGas(tx); err == nil {
		tx.Gas = estimatedGas
	}
	return c.SendRawTransaction(tx)
}

func (c *Client) mintExported(contractAddress, to, amountStr string) (interface{}, error) {
	amount, ok := new(big.Int).SetString(amountStr, 10)
	if !ok {
		return nil, fmt.Errorf("invalid amount: %s", amountStr)
	}
	return c.ERC20Mint(contractAddress, to, amount)
}

// ---------------------------------------------------------------------
// ERC20 Read Utilities: Balance, Total Supply, and Allowance
// ---------------------------------------------------------------------
func (c *Client) ERC20Balance(contractAddress, account string) (*big.Int, error) {
	parsedABI, err := abi.NewABI(erc20ReadABI)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ERC20 read ABI: %w", err)
	}
	balanceMethod, ok := parsedABI.Methods["balanceOf"]
	if !ok {
		return nil, fmt.Errorf("balanceOf method not found in ERC20 read ABI")
	}
	data, err := balanceMethod.Encode([]any{ethgo.HexToAddress(account)})
	if err != nil {
		return nil, fmt.Errorf("failed to encode balanceOf arguments: %w", err)
	}
	toAddress := ethgo.HexToAddress(contractAddress)
	callMsg := &ethgo.CallMsg{
		From: c.w.Address(),
		To:   &toAddress,
		Data: data,
	}
	result, err := c.client.Eth().Call(callMsg, ethgo.Latest)
	if err != nil {
		return nil, fmt.Errorf("failed to call balanceOf: %w", err)
	}
	fmt.Printf("balanceOf call result: %s\n", result) // Debug log
	retVals, err := balanceMethod.Outputs.Decode([]byte(result))
	if err != nil {
		return nil, fmt.Errorf("failed to decode balanceOf return: %w", err)
	}
	retValsSlice, ok := retVals.([]interface{})
	if !ok || len(retValsSlice) < 1 {
		return nil, fmt.Errorf("no return value from balanceOf")
	}
	balance, ok := retValsSlice[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("unexpected return type for balanceOf")
	}
	return balance, nil
}

func (c *Client) ERC20TotalSupply(contractAddress string) (*big.Int, error) {
	parsedABI, err := abi.NewABI(erc20ReadABI)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ERC20 read ABI: %w", err)
	}
	totalSupplyMethod, ok := parsedABI.Methods["totalSupply"]
	if !ok {
		return nil, fmt.Errorf("totalSupply method not found in ERC20 read ABI")
	}
	data, err := totalSupplyMethod.Encode([]any{})
	if err != nil {
		return nil, fmt.Errorf("failed to encode totalSupply arguments: %w", err)
	}
	toAddress := ethgo.HexToAddress(contractAddress)
	callMsg := &ethgo.CallMsg{
		From: c.w.Address(),
		To:   &toAddress,
		Data: data,
	}
	result, err := c.client.Eth().Call(callMsg, ethgo.Latest)
	if err != nil {
		return nil, fmt.Errorf("failed to call totalSupply: %w", err)
	}
	retVals, err := totalSupplyMethod.Outputs.Decode([]byte(result))
	if err != nil {
		return nil, fmt.Errorf("failed to decode totalSupply return: %w", err)
	}
	retValsSlice, ok := retVals.([]interface{})
	if !ok || len(retValsSlice) < 1 {
		return nil, fmt.Errorf("no return value from totalSupply")
	}
	retValsSlice, ok = retVals.([]interface{})
	if !ok || len(retValsSlice) < 1 {
		return nil, fmt.Errorf("no return value from totalSupply")
	}
	totalSupply, ok := retValsSlice[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("unexpected return type for totalSupply")
	}
	return totalSupply, nil
}

func (c *Client) ERC20Allowance(contractAddress, owner, spender string) (*big.Int, error) {
	parsedABI, err := abi.NewABI(erc20ReadABI)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ERC20 read ABI: %w", err)
	}
	allowanceMethod, ok := parsedABI.Methods["allowance"]
	if !ok {
		return nil, fmt.Errorf("allowance method not found in ERC20 read ABI")
	}
	data, err := allowanceMethod.Encode([]any{ethgo.HexToAddress(owner), ethgo.HexToAddress(spender)})
	if err != nil {
		return nil, fmt.Errorf("failed to encode allowance arguments: %w", err)
	}
	toAddress := ethgo.HexToAddress(contractAddress)
	callMsg := &ethgo.CallMsg{
		From: c.w.Address(),
		To:   &toAddress,
		Data: data,
	}
	result, err := c.client.Eth().Call(callMsg, ethgo.Latest)
	if err != nil {
		return nil, fmt.Errorf("failed to call allowance: %w", err)
	}
	retVals, err := allowanceMethod.Outputs.Decode([]byte(result))
	if err != nil {
		return nil, fmt.Errorf("failed to decode allowance return: %w", err)
	}
	retValsSlice, ok := retVals.([]interface{})
	if !ok || len(retValsSlice) < 1 {
		return nil, fmt.Errorf("no return value from allowance")
	}
	retValsSlice, ok = retVals.([]interface{})
	if !ok || len(retValsSlice) < 1 {
		return nil, fmt.Errorf("no return value from allowance")
	}
	allowance, ok := retValsSlice[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("unexpected return type for allowance")
	}
	return allowance, nil
}

// Exported wrappers for ERC20 read utilities.
func (c *Client) erc20BalanceExported(contractAddress, account string) (string, error) {
	balance, err := c.ERC20Balance(contractAddress, account)
	if err != nil {
		return "", err
	}
	return balance.String(), nil
}

func (c *Client) erc20TotalSupplyExported(contractAddress string) (string, error) {
	totalSupply, err := c.ERC20TotalSupply(contractAddress)
	if err != nil {
		return "", err
	}
	return totalSupply.String(), nil
}

func (c *Client) erc20AllowanceExported(contractAddress, owner, spender string) (string, error) {
	allowance, err := c.ERC20Allowance(contractAddress, owner, spender)
	if err != nil {
		return "", err
	}
	return allowance.String(), nil
}

// ---------------------------------------------------------------------
// makeHandledPromise and pollForBlocks (unchanged)
// ---------------------------------------------------------------------
func (c *Client) makeHandledPromise() (*sobek.Promise, func(interface{}), func(interface{})) {
	runtime := c.vu.Runtime()
	callback := c.vu.RegisterCallback()
	p, resolve, reject := runtime.NewPromise()
	return p, func(i interface{}) {
			callback(func() error {
				resolve(i)
				return nil
			})
		}, func(i interface{}) {
			callback(func() error {
				reject(i)
				return nil
			})
		}
}

var blocks sync.Map

func (c *Client) pollForBlocks() {
	var lastBlockNumber uint64
	var prevBlock *ethgo.Block
	now := time.Now()
	for range time.Tick(500 * time.Millisecond) {
		blockNumber, err := c.BlockNumber()
		if err != nil {
			panic(err)
		}
		if blockNumber > lastBlockNumber {
			blockTime := time.Since(now)
			now = time.Now()
			block, err := c.GetBlockByNumber(ethgo.BlockNumber(blockNumber), false)
			if err != nil {
				panic(err)
			}
			if block == nil {
				continue
			}
			lastBlockNumber = blockNumber
			var blockTimestampDiff time.Duration
			var tps float64
			if prevBlock != nil {
				blockTimestampDiff = time.Unix(int64(block.Timestamp), 0).Sub(time.Unix(int64(prevBlock.Timestamp), 0))
				tps = float64(len(block.TransactionsHashes)) / float64(blockTimestampDiff.Seconds())
			}
			prevBlock = block
			rootTS := metrics.NewRegistry().RootTagSet()
			if c.vu != nil && c.vu.State() != nil && rootTS != nil {
				if _, loaded := blocks.LoadOrStore(c.opts.URL+strconv.FormatUint(blockNumber, 10), true); loaded {
					continue
				}
				metrics.PushIfNotDone(c.vu.Context(), c.vu.State().Samples, metrics.ConnectedSamples{
					Samples: []metrics.Sample{
						{
							TimeSeries: metrics.TimeSeries{
								Metric: c.metrics.Block,
								Tags: rootTS.WithTagsFromMap(map[string]string{
									"transactions": strconv.Itoa(len(block.TransactionsHashes)),
									"gas_used":     strconv.Itoa(int(block.GasUsed)),
									"gas_limit":    strconv.Itoa(int(block.GasLimit)),
								}),
							},
							Value: float64(blockNumber),
							Time:  time.Now(),
						},
						{
							TimeSeries: metrics.TimeSeries{
								Metric: c.metrics.GasUsed,
								Tags: rootTS.WithTagsFromMap(map[string]string{
									"block": strconv.Itoa(int(blockNumber)),
								}),
							},
							Value: float64(block.GasUsed),
							Time:  time.Now(),
						},
						{
							TimeSeries: metrics.TimeSeries{
								Metric: c.metrics.TPS,
								Tags:   rootTS,
							},
							Value: tps,
							Time:  time.Now(),
						},
						{
							TimeSeries: metrics.TimeSeries{
								Metric: c.metrics.BlockTime,
								Tags: rootTS.WithTagsFromMap(map[string]string{
									"block_timestamp_diff": blockTimestampDiff.String(),
								}),
							},
							Value: float64(blockTime.Milliseconds()),
							Time:  time.Now(),
						},
					},
				})
			}
		}
	}
}
