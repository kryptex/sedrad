package server

import (
	"context"
	"fmt"
	"time"

	"github.com/sedracoin/sedrad/cmd/sedrawallet/daemon/pb"
	"github.com/sedracoin/sedrad/cmd/sedrawallet/libsedrawallet"
	"github.com/sedracoin/sedrad/domain/consensus/utils/constants"
	"github.com/sedracoin/sedrad/util"
	"github.com/pkg/errors"
	"golang.org/x/exp/slices"
)

// TODO: Implement a better fee estimation mechanism
const feePerInput = 10000

func (s *server) CreateUnsignedTransactions(_ context.Context, request *pb.CreateUnsignedTransactionsRequest) (
	*pb.CreateUnsignedTransactionsResponse, error,
) {
	s.lock.Lock()
	defer s.lock.Unlock()

	unsignedTransactions, err := s.createUnsignedTransactions(request.Address, request.Amount, request.IsSendAll,
		request.From, request.UseExistingChangeAddress)
	if err != nil {
		return nil, err
	}

	return &pb.CreateUnsignedTransactionsResponse{UnsignedTransactions: unsignedTransactions}, nil
}

func (s *server) createUnsignedTransactions(address string, amount uint64, isSendAll bool, fromAddressesString []string, useExistingChangeAddress bool) ([][]byte, error) {
	if !s.isSynced() {
		return nil, errors.Errorf("wallet daemon is not synced yet, %s", s.formatSyncStateReport())
	}

	// make sure address string is correct before proceeding to a
	// potentially long UTXO refreshment operation
	toAddress, err := util.DecodeAddress(address, s.params.Prefix)
	if err != nil {
		return nil, err
	}

	err = s.refreshUTXOs()
	if err != nil {
		return nil, err
	}

	var fromAddresses []*walletAddress
	for _, from := range fromAddressesString {
		fromAddress, exists := s.addressSet[from]
		if !exists {
			return nil, fmt.Errorf("Specified from address %s does not exists", from)
		}
		fromAddresses = append(fromAddresses, fromAddress)
	}

	selectedUTXOs, spendValue, changeSeep, err := s.selectUTXOs(amount, isSendAll, feePerInput, fromAddresses)
	if err != nil {
		return nil, err
	}

	if len(selectedUTXOs) == 0 {
		return nil, errors.Errorf("couldn't find funds to spend")
	}

	changeAddress, changeWalletAddress, err := s.changeAddress(useExistingChangeAddress, fromAddresses)
	if err != nil {
		return nil, err
	}

	payments := []*libsedrawallet.Payment{{
		Address: toAddress,
		Amount:  spendValue,
	}}
	if changeSeep > 0 {
		payments = append(payments, &libsedrawallet.Payment{
			Address: changeAddress,
			Amount:  changeSeep,
		})
	}
	unsignedTransaction, err := libsedrawallet.CreateUnsignedTransaction(s.keysFile.ExtendedPublicKeys,
		s.keysFile.MinimumSignatures,
		payments, selectedUTXOs)
	if err != nil {
		return nil, err
	}

	unsignedTransactions, err := s.maybeAutoCompoundTransaction(unsignedTransaction, toAddress, changeAddress, changeWalletAddress)
	if err != nil {
		return nil, err
	}
	return unsignedTransactions, nil
}

func (s *server) selectUTXOs(spendAmount uint64, isSendAll bool, feePerInput uint64, fromAddresses []*walletAddress) (
	selectedUTXOs []*libsedrawallet.UTXO, totalReceived uint64, changeSeep uint64, err error) {

	selectedUTXOs = []*libsedrawallet.UTXO{}
	totalValue := uint64(0)

	dagInfo, err := s.rpcClient.GetBlockDAGInfo()
	if err != nil {
		return nil, 0, 0, err
	}

	coinbaseMaturity := s.params.BlockCoinbaseMaturity
	if dagInfo.NetworkName == "sedra-testnet-11" {
		coinbaseMaturity = 1000
	}

	for _, utxo := range s.utxosSortedByAmount {
		if (fromAddresses != nil && !slices.Contains(fromAddresses, utxo.address)) ||
			!isUTXOSpendable(utxo, dagInfo.VirtualDAAScore, coinbaseMaturity) {
			continue
		}

		if broadcastTime, ok := s.usedOutpoints[*utxo.Outpoint]; ok {
			if time.Since(broadcastTime) > time.Minute {
				delete(s.usedOutpoints, *utxo.Outpoint)
			} else {
				continue
			}
		}

		selectedUTXOs = append(selectedUTXOs, &libsedrawallet.UTXO{
			Outpoint:       utxo.Outpoint,
			UTXOEntry:      utxo.UTXOEntry,
			DerivationPath: s.walletAddressPath(utxo.address),
		})

		totalValue += utxo.UTXOEntry.Amount()

		fee := feePerInput * uint64(len(selectedUTXOs))
		totalSpend := spendAmount + fee
		if !isSendAll && totalValue >= totalSpend {
			break
		}
	}

	fee := feePerInput * uint64(len(selectedUTXOs))
	var totalSpend uint64
	if isSendAll {
		totalSpend = totalValue
		totalReceived = totalValue - fee
	} else {
		totalSpend = spendAmount + fee
		totalReceived = spendAmount
	}
	if totalValue < totalSpend {
		return nil, 0, 0, errors.Errorf("Insufficient funds for send: %f required, while only %f available",
			float64(totalSpend)/constants.SeepPerSedra, float64(totalValue)/constants.SeepPerSedra)
	}

	return selectedUTXOs, totalReceived, totalValue - totalSpend, nil
}
