package relayer

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"log/slog"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/pkg/errors"
	"github.com/taikoxyz/taiko-mono/packages/relayer/bindings/erc1155vault"
	"github.com/taikoxyz/taiko-mono/packages/relayer/bindings/erc20vault"
	"github.com/taikoxyz/taiko-mono/packages/relayer/bindings/erc721vault"
	"github.com/taikoxyz/taiko-mono/packages/relayer/bindings/signalservice"
)

var (
	ZeroHash    = common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000")
	ZeroAddress = common.HexToAddress("0x0000000000000000000000000000000000000000")
)

// IsInSlice determines whether v is in slice s
func IsInSlice[T comparable](v T, s []T) bool {
	for _, e := range s {
		if v == e {
			return true
		}
	}

	return false
}

type confirmer interface {
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
	BlockNumber(ctx context.Context) (uint64, error)
}

// WaitReceipt keeps waiting until the given transaction has an execution
// receipt to know whether it was reverted or not.
func WaitReceipt(ctx context.Context, confirmer confirmer, txHash common.Hash) (*types.Receipt, error) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	slog.Info("waiting for transaction receipt", "txHash", txHash.Hex())

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			receipt, err := confirmer.TransactionReceipt(ctx, txHash)
			if err != nil {
				continue
			}

			if receipt.Status != types.ReceiptStatusSuccessful {
				ssabi, originalError := signalservice.SignalServiceMetaData.GetAbi()
				if originalError != nil {
					log.Crit("Get AssignmentHook ABI error", "error", originalError)
				}

				var customErrorMaps = []map[string]abi.Error{
					ssabi.Errors,
				}

				errData := getErrorData(originalError)

				// if errData is unparsable and returns 0x, we should not match any errors.
				if errData == "0x" {
					return receipt, originalError
				}

				for _, customErrors := range customErrorMaps {
					for _, customError := range customErrors {
						if strings.HasPrefix(customError.ID.Hex(), errData) {
							return receipt, errors.New(customError.Name)
						}
					}
				}

				return nil, fmt.Errorf("transaction reverted, hash: %s", txHash)
			}

			slog.Info("transaction receipt found", "txHash", txHash.Hex())

			return receipt, nil
		}
	}
}

// getErrorData tries to parse the actual custom error data from the given error.
func getErrorData(err error) string {
	// Geth node custom errors, the actual struct of this error is go-ethereum's <rpc.jsonError Value>.
	gethJSONError, ok := err.(interface{ ErrorData() interface{} }) // nolint: errorlint
	if ok {
		if errData, ok := gethJSONError.ErrorData().(string); ok {
			return errData
		}
	}

	// Hardhat node custom errors, example:
	// "VM Exception while processing transaction: reverted with an unrecognized custom error (return data: 0xb6d363fd)"
	if strings.Contains(err.Error(), "reverted with an unrecognized custom error") {
		return err.Error()[len(err.Error())-11 : len(err.Error())-1]
	}

	return err.Error()
}

// WaitConfirmations won't return before N blocks confirmations have been seen
// on destination chain, or context is cancelled.
func WaitConfirmations(ctx context.Context, confirmer confirmer, confirmations uint64, txHash common.Hash) error {
	slog.Info("beginning waiting for confirmations", "txHash", txHash.Hex())

	ticker := time.NewTicker(10 * time.Second)

	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			receipt, err := confirmer.TransactionReceipt(ctx, txHash)
			if err != nil {
				if err == ethereum.NotFound {
					continue
				}

				slog.Error("encountered error getting receipt", "txHash", txHash.Hex(), "error", err)

				return err
			}

			latest, err := confirmer.BlockNumber(ctx)
			if err != nil {
				return err
			}

			want := receipt.BlockNumber.Uint64() + confirmations
			slog.Info(
				"waiting for confirmations",
				"txHash", txHash.Hex(),
				"confirmations", confirmations,
				"blockNumWillbeConfirmed", want,
				"latestBlockNum", latest,
			)

			if latest < receipt.BlockNumber.Uint64()+confirmations {
				continue
			}

			slog.Info("done waiting for confirmations", "txHash", txHash.Hex(), "confirmations", confirmations)

			return nil
		}
	}
}

// DecodeMessageData tries to tell if it's an ETH, ERC20, ERC721, or ERC1155 bridge,
// which lets the processor look up whether the contract has already been deployed or not,
// to help better estimate gas needed for processing the message.
func DecodeMessageData(eventData []byte, value *big.Int) (EventType, CanonicalToken, *big.Int, error) {
	eventType := EventTypeSendETH

	var canonicalToken CanonicalToken

	var amount *big.Int = big.NewInt(0)

	erc20ReceiveTokensFunctionSig := "240f6a5f"
	erc721ReceiveTokensFunctionSig := "300536b5"
	erc1155ReceiveTokensFunctionSig := "079312bf"

	// try to see if its an ERC20
	if eventData != nil && common.BytesToHash(eventData) != ZeroHash && len(eventData) > 3 {
		functionSig := eventData[:4]

		if common.Bytes2Hex(functionSig) == erc20ReceiveTokensFunctionSig {
			erc20VaultMD := bind.MetaData{
				ABI: erc20vault.ERC20VaultABI,
			}

			erc20VaultABI, err := erc20VaultMD.GetAbi()
			if err != nil {
				return eventType, nil, big.NewInt(0), errors.Wrap(err, "erc20VaultMD.GetAbi()")
			}

			method, err := erc20VaultABI.MethodById(eventData[:4])
			if err != nil {
				return eventType, nil, big.NewInt(0), errors.Wrap(err, "tokenVaultABI.MethodById")
			}

			inputsMap := make(map[string]interface{})

			if err := method.Inputs.UnpackIntoMap(inputsMap, eventData[4:]); err != nil {
				return eventType, nil, big.NewInt(0), errors.Wrap(err, "method.Inputs.UnpackIntoMap")
			}

			if method.Name == "receiveToken" {
				eventType = EventTypeSendERC20

				// have to unpack to anonymous struct first due to abi limitation
				t := inputsMap["ctoken"].(struct {
					// nolint
					ChainId  uint64         `json:"chainId"`
					Addr     common.Address `json:"addr"`
					Decimals uint8          `json:"decimals"`
					Symbol   string         `json:"symbol"`
					Name     string         `json:"name"`
				})

				canonicalToken = CanonicalERC20{
					ChainId:  t.ChainId,
					Addr:     t.Addr,
					Decimals: t.Decimals,
					Symbol:   t.Symbol,
					Name:     t.Name,
				}

				amount = inputsMap["amount"].(*big.Int)
			}
		}

		if common.Bytes2Hex(functionSig) == erc721ReceiveTokensFunctionSig {
			erc721VaultMD := bind.MetaData{
				ABI: erc721vault.ERC721VaultABI,
			}

			erc721VaultABI, err := erc721VaultMD.GetAbi()
			if err != nil {
				return eventType, nil, big.NewInt(0), errors.Wrap(err, "erc20VaultMD.GetAbi()")
			}

			method, err := erc721VaultABI.MethodById(eventData[:4])
			if err != nil {
				return eventType, nil, big.NewInt(0), errors.Wrap(err, "tokenVaultABI.MethodById")
			}

			inputsMap := make(map[string]interface{})

			if err := method.Inputs.UnpackIntoMap(inputsMap, eventData[4:]); err != nil {
				return eventType, nil, big.NewInt(0), errors.Wrap(err, "method.Inputs.UnpackIntoMap")
			}

			if method.Name == "receiveToken" {
				eventType = EventTypeSendERC721

				t := inputsMap["ctoken"].(struct {
					// nolint
					ChainId uint64         `json:"chainId"`
					Addr    common.Address `json:"addr"`
					Symbol  string         `json:"symbol"`
					Name    string         `json:"name"`
				})

				canonicalToken = CanonicalNFT{
					ChainId: t.ChainId,
					Addr:    t.Addr,
					Symbol:  t.Symbol,
					Name:    t.Name,
				}

				amount = big.NewInt(1)
			}
		}

		if common.Bytes2Hex(functionSig) == erc1155ReceiveTokensFunctionSig {
			erc1155VaultMD := bind.MetaData{
				ABI: erc1155vault.ERC1155VaultABI,
			}

			erc1155VaultABI, err := erc1155VaultMD.GetAbi()
			if err != nil {
				return eventType, nil, big.NewInt(0), errors.Wrap(err, "erc1155VaultMD.GetAbi()")
			}

			method, err := erc1155VaultABI.MethodById(eventData[:4])
			if err != nil {
				return eventType, nil, big.NewInt(0), errors.Wrap(err, "tokenVaultABI.MethodById")
			}

			inputsMap := make(map[string]interface{})

			if err := method.Inputs.UnpackIntoMap(inputsMap, eventData[4:]); err != nil {
				return eventType, nil, big.NewInt(0), errors.Wrap(err, "method.Inputs.UnpackIntoMap")
			}

			if method.Name == "receiveToken" {
				eventType = EventTypeSendERC1155

				t := inputsMap["ctoken"].(struct {
					// nolint
					ChainId uint64         `json:"chainId"`
					Addr    common.Address `json:"addr"`
					Symbol  string         `json:"symbol"`
					Name    string         `json:"name"`
				})

				canonicalToken = CanonicalNFT{
					ChainId: t.ChainId,
					Addr:    t.Addr,
					Symbol:  t.Symbol,
					Name:    t.Name,
				}

				amounts := inputsMap["amounts"].([]*big.Int)

				for _, v := range amounts {
					amount = amount.Add(amount, v)
				}
			}
		}
	} else {
		amount = value
	}

	return eventType, canonicalToken, amount, nil
}

type CanonicalToken interface {
	ChainID() uint64
	Address() common.Address
	ContractName() string
	TokenDecimals() uint8
	ContractSymbol() string
}

type CanonicalERC20 struct {
	// nolint
	ChainId  uint64         `json:"chainId"`
	Addr     common.Address `json:"addr"`
	Decimals uint8          `json:"decimals"`
	Symbol   string         `json:"symbol"`
	Name     string         `json:"name"`
}

func (c CanonicalERC20) ChainID() uint64 {
	return c.ChainId
}

func (c CanonicalERC20) Address() common.Address {
	return c.Addr
}

func (c CanonicalERC20) ContractName() string {
	return c.Name
}

func (c CanonicalERC20) ContractSymbol() string {
	return c.Symbol
}

func (c CanonicalERC20) TokenDecimals() uint8 {
	return c.Decimals
}

type CanonicalNFT struct {
	// nolint
	ChainId uint64         `json:"chainId"`
	Addr    common.Address `json:"addr"`
	Symbol  string         `json:"symbol"`
	Name    string         `json:"name"`
}

func (c CanonicalNFT) ChainID() uint64 {
	return c.ChainId
}

func (c CanonicalNFT) Address() common.Address {
	return c.Addr
}

func (c CanonicalNFT) ContractName() string {
	return c.Name
}

func (c CanonicalNFT) TokenDecimals() uint8 {
	return 0
}

func (c CanonicalNFT) ContractSymbol() string {
	return c.Symbol
}
