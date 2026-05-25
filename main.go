package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	x402 "github.com/x402-foundation/x402/go"
	evmmech "github.com/x402-foundation/x402/go/mechanisms/evm"
	evmfacilitator "github.com/x402-foundation/x402/go/mechanisms/evm/exact/facilitator"
)

type config struct {
	listenAddr       string
	rpcURL           string
	network          x402.Network
	privateKey       string
	verifyTimeout    time.Duration
	settleTimeout    time.Duration
	receiptTimeout   time.Duration
	deployERC4337    bool
	simulateInSettle bool
}

type server struct {
	cfg         config
	facilitator x402Facilitator
	signer      *facilitatorEvmSigner
}

type x402Facilitator interface {
	GetSupported() x402.SupportedResponse
	Verify(context.Context, []byte, []byte) (*x402.VerifyResponse, error)
	Settle(context.Context, []byte, []byte) (*x402.SettleResponse, error)
}

type facilitatorRequest struct {
	PaymentPayload      json.RawMessage `json:"paymentPayload"`
	PaymentRequirements json.RawMessage `json:"paymentRequirements"`
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	signer, err := newFacilitatorEvmSigner(cfg.privateKey, cfg.rpcURL, cfg.receiptTimeout)
	if err != nil {
		log.Fatalf("create evm signer: %v", err)
	}

	facilitator := x402.Newx402Facilitator()
	facilitator.Register(
		[]x402.Network{cfg.network},
		evmfacilitator.NewExactEvmScheme(signer, &evmfacilitator.ExactEvmSchemeConfig{
			DeployERC4337WithEIP6492: cfg.deployERC4337,
			SimulateInSettle:         cfg.simulateInSettle,
		}),
	)
	facilitator.OnAfterVerify(func(ctx x402.FacilitatorVerifyResultContext) error {
		log.Printf("x402 verified payer=%s network=%s", ctx.Result.Payer, ctx.Requirements.GetNetwork())
		return nil
	})
	facilitator.OnAfterSettle(func(ctx x402.FacilitatorSettleResultContext) error {
		log.Printf("x402 settled payer=%s network=%s tx=%s", ctx.Result.Payer, ctx.Result.Network, ctx.Result.Transaction)
		return nil
	})
	facilitator.OnVerifyFailure(func(ctx x402.FacilitatorVerifyFailureContext) (*x402.FacilitatorVerifyFailureHookResult, error) {
		log.Printf("x402 verify failed: %v", ctx.Error)
		return nil, nil
	})
	facilitator.OnSettleFailure(func(ctx x402.FacilitatorSettleFailureContext) (*x402.FacilitatorSettleFailureHookResult, error) {
		log.Printf("x402 settle failed: %v", ctx.Error)
		return nil, nil
	})

	srv := &server{cfg: cfg, facilitator: facilitator, signer: signer}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.health)
	mux.HandleFunc("/supported", srv.supported)
	mux.HandleFunc("/verify", srv.verify)
	mux.HandleFunc("/settle", srv.settle)
	mux.HandleFunc("/v1/x402/supported", srv.supported)
	mux.HandleFunc("/v1/x402/verify", srv.verify)
	mux.HandleFunc("/v1/x402/settle", srv.settle)

	log.Printf(
		"sub2api official x402 facilitator listening on %s network=%s signer=%s rpc_chain_id=%s",
		cfg.listenAddr,
		cfg.network,
		signer.address.Hex(),
		signer.chainID.String(),
	)
	log.Fatal(http.ListenAndServe(cfg.listenAddr, mux))
}

func loadConfig() (config, error) {
	key := firstNonEmpty(os.Getenv("FACILITATOR_PRIVATE_KEY"), os.Getenv("EVM_PRIVATE_KEY"))
	if strings.TrimSpace(key) == "" {
		return config{}, errors.New("FACILITATOR_PRIVATE_KEY or EVM_PRIVATE_KEY is required")
	}

	rpcURL := firstNonEmpty(os.Getenv("RPC_URL"), os.Getenv("BASE_RPC_URL"), os.Getenv("SEPOLIA_RPC_URL"))
	if strings.TrimSpace(rpcURL) == "" {
		return config{}, errors.New("RPC_URL is required")
	}

	chainID := parseInt64Env("CHAIN_ID", 8453)
	network := firstNonEmpty(os.Getenv("X402_NETWORK"), os.Getenv("NETWORK"))
	if strings.TrimSpace(network) == "" {
		network = fmt.Sprintf("eip155:%d", chainID)
	}

	return config{
		listenAddr:       firstNonEmpty(os.Getenv("LISTEN_ADDR"), ":18081"),
		rpcURL:           strings.TrimSpace(rpcURL),
		network:          x402.Network(strings.TrimSpace(network)),
		privateKey:       strings.TrimSpace(key),
		verifyTimeout:    parseDurationSecondsEnv("VERIFY_TIMEOUT_SECONDS", 30*time.Second),
		settleTimeout:    parseDurationSecondsEnv("SETTLE_TIMEOUT_SECONDS", 120*time.Second),
		receiptTimeout:   parseDurationSecondsEnv("CONFIRM_TIMEOUT_SECONDS", 120*time.Second),
		deployERC4337:    parseBoolEnv("DEPLOY_ERC4337_WITH_EIP6492", true),
		simulateInSettle: parseBoolEnv("SIMULATE_IN_SETTLE", false),
	}, nil
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"status":  "ok",
		"network": string(s.cfg.network),
		"signer":  s.signer.address.Hex(),
	})
}

func (s *server) supported(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	writeJSON(w, http.StatusOK, s.facilitator.GetSupported())
}

func (s *server) verify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}

	req, ok := readFacilitatorRequest(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.verifyTimeout)
	defer cancel()

	result, err := s.facilitator.Verify(ctx, req.PaymentPayload, req.PaymentRequirements)
	if err != nil {
		writeVerifyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) settle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}

	req, ok := readFacilitatorRequest(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.settleTimeout)
	defer cancel()

	result, err := s.facilitator.Settle(ctx, req.PaymentPayload, req.PaymentRequirements)
	if err != nil {
		writeSettleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func readFacilitatorRequest(w http.ResponseWriter, r *http.Request) (facilitatorRequest, bool) {
	var req facilitatorRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return req, false
	}
	if len(bytes.TrimSpace(req.PaymentPayload)) == 0 || len(bytes.TrimSpace(req.PaymentRequirements)) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_payment_payload_or_requirements"})
		return req, false
	}
	return req, true
}

func writeVerifyError(w http.ResponseWriter, err error) {
	var verifyErr *x402.VerifyError
	if errors.As(err, &verifyErr) {
		writeJSON(w, http.StatusOK, x402.VerifyResponse{
			IsValid:        false,
			InvalidReason:  verifyErr.InvalidReason,
			InvalidMessage: verifyErr.InvalidMessage,
			Payer:          verifyErr.Payer,
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

func writeSettleError(w http.ResponseWriter, err error) {
	var settleErr *x402.SettleError
	if errors.As(err, &settleErr) {
		writeJSON(w, http.StatusOK, x402.SettleResponse{
			Success:      false,
			ErrorReason:  settleErr.ErrorReason,
			ErrorMessage: settleErr.ErrorMessage,
			Payer:        settleErr.Payer,
			Network:      settleErr.Network,
			Transaction:  settleErr.Transaction,
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type facilitatorEvmSigner struct {
	privateKey     *ecdsa.PrivateKey
	address        common.Address
	client         *ethclient.Client
	chainID        *big.Int
	receiptTimeout time.Duration
}

func newFacilitatorEvmSigner(privateKeyHex string, rpcURL string, receiptTimeout time.Duration) (*facilitatorEvmSigner, error) {
	privateKeyHex = strings.TrimPrefix(strings.TrimSpace(privateKeyHex), "0x")
	privateKey, err := crypto.HexToECDSA(privateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		return nil, fmt.Errorf("dial rpc: %w", err)
	}

	chainID, err := client.ChainID(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get chain id: %w", err)
	}

	return &facilitatorEvmSigner{
		privateKey:     privateKey,
		address:        crypto.PubkeyToAddress(privateKey.PublicKey),
		client:         client,
		chainID:        chainID,
		receiptTimeout: receiptTimeout,
	}, nil
}

func (s *facilitatorEvmSigner) GetAddresses() []string {
	return []string{s.address.Hex()}
}

func (s *facilitatorEvmSigner) GetChainID(context.Context) (*big.Int, error) {
	return new(big.Int).Set(s.chainID), nil
}

func (s *facilitatorEvmSigner) VerifyTypedData(
	ctx context.Context,
	address string,
	domain evmmech.TypedDataDomain,
	types map[string][]evmmech.TypedDataField,
	primaryType string,
	message map[string]interface{},
	signature []byte,
) (bool, error) {
	chainID := getBigIntFromInterface(domain.ChainID)
	typedData := apitypes.TypedData{
		Types:       make(apitypes.Types),
		PrimaryType: primaryType,
		Domain: apitypes.TypedDataDomain{
			Name:              domain.Name,
			Version:           domain.Version,
			ChainId:           (*ethmath.HexOrDecimal256)(chainID),
			VerifyingContract: domain.VerifyingContract,
		},
		Message: message,
	}

	for typeName, fields := range types {
		typedFields := make([]apitypes.Type, len(fields))
		for i, field := range fields {
			typedFields[i] = apitypes.Type{Name: field.Name, Type: field.Type}
		}
		typedData.Types[typeName] = typedFields
	}

	if _, exists := typedData.Types["EIP712Domain"]; !exists {
		typedData.Types["EIP712Domain"] = []apitypes.Type{
			{Name: "name", Type: "string"},
			{Name: "version", Type: "string"},
			{Name: "chainId", Type: "uint256"},
			{Name: "verifyingContract", Type: "address"},
		}
	}

	dataHash, err := typedData.HashStruct(typedData.PrimaryType, typedData.Message)
	if err != nil {
		return false, fmt.Errorf("hash typed data struct: %w", err)
	}
	domainSeparator, err := typedData.HashStruct("EIP712Domain", typedData.Domain.Map())
	if err != nil {
		return false, fmt.Errorf("hash typed data domain: %w", err)
	}

	digest := crypto.Keccak256(append(append([]byte{0x19, 0x01}, domainSeparator...), dataHash...))
	if len(signature) != 65 {
		return false, fmt.Errorf("invalid signature length: %d", len(signature))
	}

	sigCopy := make([]byte, 65)
	copy(sigCopy, signature)
	if sigCopy[64] >= 27 {
		sigCopy[64] -= 27
	}

	pubKey, err := crypto.SigToPub(digest, sigCopy)
	if err != nil {
		return false, fmt.Errorf("recover signature: %w", err)
	}

	recovered := crypto.PubkeyToAddress(*pubKey)
	return bytes.Equal(recovered.Bytes(), common.HexToAddress(address).Bytes()), nil
}

func (s *facilitatorEvmSigner) ReadContract(
	ctx context.Context,
	contractAddress string,
	abiJSON []byte,
	method string,
	args ...interface{},
) (interface{}, error) {
	contractABI, err := abi.JSON(strings.NewReader(string(abiJSON)))
	if err != nil {
		return nil, fmt.Errorf("parse abi: %w", err)
	}

	methodObj, exists := contractABI.Methods[method]
	if !exists {
		return nil, fmt.Errorf("method %s not found in abi", method)
	}

	data, err := contractABI.Pack(method, args...)
	if err != nil {
		return nil, fmt.Errorf("pack contract call: %w", err)
	}

	to := common.HexToAddress(contractAddress)
	result, err := s.client.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("call contract: %w", err)
	}

	if len(methodObj.Outputs) == 0 {
		return nil, nil
	}
	output, err := methodObj.Outputs.Unpack(result)
	if err != nil {
		return nil, fmt.Errorf("unpack contract result: %w", err)
	}
	if len(output) == 0 {
		return nil, nil
	}
	return output[0], nil
}

func (s *facilitatorEvmSigner) WriteContract(
	ctx context.Context,
	address string,
	abiJSON []byte,
	functionName string,
	args ...interface{},
) (string, error) {
	contractABI, err := abi.JSON(strings.NewReader(string(abiJSON)))
	if err != nil {
		return "", fmt.Errorf("parse abi: %w", err)
	}
	data, err := contractABI.Pack(functionName, args...)
	if err != nil {
		return "", fmt.Errorf("pack contract call: %w", err)
	}
	return s.sendTransaction(ctx, common.HexToAddress(address), data)
}

func (s *facilitatorEvmSigner) SendTransaction(ctx context.Context, to string, data []byte) (string, error) {
	return s.sendTransaction(ctx, common.HexToAddress(to), data)
}

func (s *facilitatorEvmSigner) sendTransaction(ctx context.Context, to common.Address, data []byte) (string, error) {
	nonce, err := s.client.PendingNonceAt(ctx, s.address)
	if err != nil {
		return "", fmt.Errorf("get nonce: %w", err)
	}

	call := ethereum.CallMsg{From: s.address, To: &to, Data: data}
	gasLimit, err := s.client.EstimateGas(ctx, call)
	if err != nil {
		return "", fmt.Errorf("estimate gas: %w", err)
	}
	gasLimit = gasLimit + gasLimit/5
	if gasLimit < 300000 {
		gasLimit = 300000
	}

	tipCap, err := s.client.SuggestGasTipCap(ctx)
	if err != nil {
		return "", fmt.Errorf("suggest gas tip cap: %w", err)
	}
	header, err := s.client.HeaderByNumber(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("get latest header: %w", err)
	}
	feeCap := new(big.Int).Add(new(big.Int).Mul(header.BaseFee, big.NewInt(2)), tipCap)

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   new(big.Int).Set(s.chainID),
		Nonce:     nonce,
		GasTipCap: tipCap,
		GasFeeCap: feeCap,
		Gas:       gasLimit,
		To:        &to,
		Value:     big.NewInt(0),
		Data:      data,
	})

	signedTx, err := types.SignTx(tx, types.LatestSignerForChainID(s.chainID), s.privateKey)
	if err != nil {
		return "", fmt.Errorf("sign transaction: %w", err)
	}
	if err := s.client.SendTransaction(ctx, signedTx); err != nil {
		return "", fmt.Errorf("send transaction: %w", err)
	}
	return signedTx.Hash().Hex(), nil
}

func (s *facilitatorEvmSigner) WaitForTransactionReceipt(ctx context.Context, txHash string) (*evmmech.TransactionReceipt, error) {
	hash := common.HexToHash(txHash)
	deadline := time.Now().Add(s.receiptTimeout)
	for {
		receipt, err := s.client.TransactionReceipt(ctx, hash)
		if err == nil && receipt != nil {
			return &evmmech.TransactionReceipt{
				Status:      uint64(receipt.Status),
				BlockNumber: receipt.BlockNumber.Uint64(),
				TxHash:      receipt.TxHash.Hex(),
			}, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("transaction receipt not found before timeout: %s", txHash)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (s *facilitatorEvmSigner) GetBalance(ctx context.Context, address string, tokenAddress string) (*big.Int, error) {
	if strings.TrimSpace(tokenAddress) == "" || strings.EqualFold(tokenAddress, "0x0000000000000000000000000000000000000000") {
		balance, err := s.client.BalanceAt(ctx, common.HexToAddress(address), nil)
		if err != nil {
			return nil, fmt.Errorf("get native balance: %w", err)
		}
		return balance, nil
	}

	const erc20BalanceABI = `[{"constant":true,"inputs":[{"name":"account","type":"address"}],"name":"balanceOf","outputs":[{"name":"","type":"uint256"}],"type":"function"}]`
	result, err := s.ReadContract(ctx, tokenAddress, []byte(erc20BalanceABI), "balanceOf", common.HexToAddress(address))
	if err != nil {
		return nil, err
	}
	balance, ok := result.(*big.Int)
	if !ok {
		return nil, fmt.Errorf("unexpected balance type: %T", result)
	}
	return balance, nil
}

func (s *facilitatorEvmSigner) GetCode(ctx context.Context, address string) ([]byte, error) {
	code, err := s.client.CodeAt(ctx, common.HexToAddress(address), nil)
	if err != nil {
		return nil, fmt.Errorf("get code: %w", err)
	}
	return code, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func parseInt64Env(name string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func parseDurationSecondsEnv(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func parseBoolEnv(name string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return value
}

func getBigIntFromInterface(v interface{}) *big.Int {
	switch val := v.(type) {
	case nil:
		return big.NewInt(0)
	case *big.Int:
		return val
	case int64:
		return big.NewInt(val)
	case string:
		n, ok := new(big.Int).SetString(val, 10)
		if ok {
			return n
		}
	}
	return big.NewInt(0)
}
