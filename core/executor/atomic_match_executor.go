package executor

import (
	"bytes"
	"encoding/json"
	"math/big"

	"github.com/pkg/errors"
	"github.com/zeromicro/go-zero/core/logx"

	"github.com/bnb-chain/zkbnb-crypto/ffmath"
	"github.com/bnb-chain/zkbnb-crypto/wasm/txtypes"
	common2 "github.com/bnb-chain/zkbnb/common"
	"github.com/bnb-chain/zkbnb/dao/tx"
	"github.com/bnb-chain/zkbnb/types"
)

type AtomicMatchExecutor struct {
	BaseExecutor

	txInfo *txtypes.AtomicMatchTxInfo

	buyOffer         *txtypes.OfferTxInfo
	sellOffer        *txtypes.OfferTxInfo
	buyOfferAssetId  int64
	buyOfferIndex    int64
	sellOfferAssetId int64
	sellOfferIndex   int64
}

func NewAtomicMatchExecutor(bc IBlockchain, tx *tx.Tx) (TxExecutor, error) {
	txInfo, err := types.ParseAtomicMatchTxInfo(tx.TxInfo)
	if err != nil {
		logx.Errorf("parse atomic match tx failed: %s", err.Error())
		return nil, errors.New("invalid tx info")
	}

	return &AtomicMatchExecutor{
		BaseExecutor: NewBaseExecutor(bc, tx, txInfo),
		txInfo:       txInfo,
	}, nil
}

func (e *AtomicMatchExecutor) Prepare() error {
	txInfo := e.txInfo

	counterpartOffer := &txtypes.OfferTxInfo{
		Type:         txInfo.Type,
		OfferId:      txInfo.OfferId,
		AccountIndex: txInfo.AccountIndex,
		NftIndex:     txInfo.NftIndex,
		AssetId:      txInfo.AssetId,
		AssetAmount:  txInfo.AssetAmount,
		ListedAt:     0,
		ExpiredAt:    txInfo.ExpiredAt,
		TreasuryRate: txInfo.TreasuryRate,
		Sig:          nil,
	}
	if txInfo.Offer.Type == types.BuyOfferType {
		e.buyOffer = txInfo.Offer
		e.sellOffer = counterpartOffer
	} else {
		e.sellOffer = txInfo.Offer
		e.buyOffer = counterpartOffer
	}

	e.buyOfferAssetId = e.buyOffer.OfferId / OfferPerAsset
	e.buyOfferIndex = e.buyOffer.OfferId % OfferPerAsset
	e.sellOfferAssetId = e.sellOffer.OfferId / OfferPerAsset
	e.sellOfferIndex = e.sellOffer.OfferId % OfferPerAsset

	// Prepare seller's asset and nft, if the buyer's asset or nft isn't the same, it will be failed in the verify step.
	matchNft, err := e.bc.StateDB().PrepareNft(txInfo.NftIndex)
	if err != nil {
		logx.Errorf("prepare nft failed")
		return errors.New("internal error")
	}

	// Set the right treasury and creator treasury amount.
	txInfo.TreasuryAmount = ffmath.Div(ffmath.Multiply(e.sellOffer.AssetAmount, big.NewInt(e.sellOffer.TreasuryRate)), big.NewInt(TenThousand))
	txInfo.CreatorAmount = ffmath.Div(ffmath.Multiply(e.sellOffer.AssetAmount, big.NewInt(matchNft.CreatorTreasuryRate)), big.NewInt(TenThousand))

	// Mark the tree states that would be affected in this executor.
	e.MarkNftDirty(e.sellOffer.NftIndex)
	e.MarkAccountAssetsDirty(txInfo.AccountIndex, []int64{txInfo.GasFeeAssetId})
	e.MarkAccountAssetsDirty(e.buyOffer.AccountIndex, []int64{e.buyOffer.AssetId, e.buyOfferAssetId})
	e.MarkAccountAssetsDirty(e.sellOffer.AccountIndex, []int64{e.sellOffer.AssetId, e.sellOfferAssetId})
	e.MarkAccountAssetsDirty(matchNft.CreatorAccountIndex, []int64{e.buyOffer.AssetId})
	e.MarkAccountAssetsDirty(txInfo.GasAccountIndex, []int64{e.buyOffer.AssetId, txInfo.GasFeeAssetId})
	return e.BaseExecutor.Prepare()
}

func (e *AtomicMatchExecutor) VerifyInputs(skipGasAmtChk bool) error {
	bc := e.bc
	txInfo := e.txInfo

	err := e.BaseExecutor.VerifyInputs(skipGasAmtChk)
	if err != nil {
		return err
	}

	if e.buyOffer.Type != types.BuyOfferType ||
		e.sellOffer.Type != types.SellOfferType {
		return errors.New("invalid offer type")
	}
	if e.buyOffer.AccountIndex == e.sellOffer.AccountIndex {
		return errors.New("same buyer and seller")
	}
	if e.sellOffer.NftIndex != e.buyOffer.NftIndex ||
		e.sellOffer.AssetId != e.buyOffer.AssetId ||
		e.sellOffer.AssetAmount.String() != e.buyOffer.AssetAmount.String() ||
		e.sellOffer.TreasuryRate != e.buyOffer.TreasuryRate {
		return errors.New("buy offer mismatches sell offer")
	}

	// only gas assets are allowed for atomic match
	found := false
	for _, assetId := range types.GasAssets {
		if assetId == e.sellOffer.AssetId {
			found = true
		}
	}
	if !found {
		return errors.New("invalid asset of offer")
	}

	// Check offer expired time.
	if err := e.bc.VerifyExpiredAt(txInfo.Offer.ExpiredAt); err != nil {
		return errors.New("invalid Offer.ExpiredAt")
	}

	fromAccount, err := bc.StateDB().GetFormatAccount(txInfo.AccountIndex)
	if err != nil {
		return err
	}
	buyAccount, err := bc.StateDB().GetFormatAccount(e.buyOffer.AccountIndex)
	if err != nil {
		return err
	}
	sellAccount, err := bc.StateDB().GetFormatAccount(e.sellOffer.AccountIndex)
	if err != nil {
		return err
	}

	// Check sender's gas balance and buyer's asset balance.
	if txInfo.AccountIndex == e.buyOffer.AccountIndex && txInfo.GasFeeAssetId == e.sellOffer.AssetId {
		totalBalance := ffmath.Add(txInfo.GasFeeAssetAmount, e.buyOffer.AssetAmount)
		if fromAccount.AssetInfo[txInfo.GasFeeAssetId].Balance.Cmp(totalBalance) < 0 {
			return errors.New("sender balance is not enough")
		}
	} else {
		if fromAccount.AssetInfo[txInfo.GasFeeAssetId].Balance.Cmp(txInfo.GasFeeAssetAmount) < 0 {
			return errors.New("sender balance is not enough")
		}

		if buyAccount.AssetInfo[e.buyOffer.AssetId].Balance.Cmp(e.buyOffer.AssetAmount) < 0 {
			return errors.New("buy balance is not enough")
		}
	}

	// Check offer canceled or finalized.
	sellOffer := sellAccount.AssetInfo[e.sellOfferAssetId].OfferCanceledOrFinalized
	if sellOffer.Bit(int(e.sellOfferIndex)) == 1 {
		return errors.New("sell offer canceled or finalized")
	}
	buyOffer := buyAccount.AssetInfo[e.buyOfferAssetId].OfferCanceledOrFinalized
	if buyOffer.Bit(int(e.buyOfferIndex)) == 1 {
		return errors.New("buy offer canceled or finalized")
	}

	// Check the seller is the owner of the nft.
	nft, err := bc.StateDB().GetNft(e.sellOffer.NftIndex)
	if err != nil {
		return err
	}
	if nft.OwnerAccountIndex != e.sellOffer.AccountIndex {
		return errors.New("seller is not owner")
	}

	// Verify offer signature.
	err = txInfo.Offer.VerifySignature(buyAccount.PublicKey)
	if err != nil {
		return err
	}

	return nil
}

func (e *AtomicMatchExecutor) ApplyTransaction() error {
	bc := e.bc
	txInfo := e.txInfo

	// apply changes
	matchNft, err := bc.StateDB().GetNft(e.sellOffer.NftIndex)
	if err != nil {
		return err
	}
	fromAccount, err := bc.StateDB().GetFormatAccount(txInfo.AccountIndex)
	if err != nil {
		return err
	}
	buyAccount, err := bc.StateDB().GetFormatAccount(e.buyOffer.AccountIndex)
	if err != nil {
		return err
	}
	sellAccount, err := bc.StateDB().GetFormatAccount(e.sellOffer.AccountIndex)
	if err != nil {
		return err
	}
	creatorAccount, err := bc.StateDB().GetFormatAccount(matchNft.CreatorAccountIndex)
	if err != nil {
		return err
	}

	fromAccount.AssetInfo[txInfo.GasFeeAssetId].Balance = ffmath.Sub(fromAccount.AssetInfo[txInfo.GasFeeAssetId].Balance, txInfo.GasFeeAssetAmount)
	buyAccount.AssetInfo[e.buyOffer.AssetId].Balance = ffmath.Sub(buyAccount.AssetInfo[e.buyOffer.AssetId].Balance, e.buyOffer.AssetAmount)
	sellAccount.AssetInfo[e.sellOffer.AssetId].Balance = ffmath.Add(sellAccount.AssetInfo[e.sellOffer.AssetId].Balance, ffmath.Sub(
		e.buyOffer.AssetAmount, ffmath.Add(txInfo.TreasuryAmount, txInfo.CreatorAmount)))
	creatorAccount.AssetInfo[e.buyOffer.AssetId].Balance = ffmath.Add(creatorAccount.AssetInfo[e.buyOffer.AssetId].Balance, txInfo.CreatorAmount)
	fromAccount.Nonce++

	sellOffer := sellAccount.AssetInfo[e.sellOfferAssetId].OfferCanceledOrFinalized
	sellOffer = new(big.Int).SetBit(sellOffer, int(e.sellOfferIndex), 1)
	sellAccount.AssetInfo[e.sellOfferAssetId].OfferCanceledOrFinalized = sellOffer
	buyOffer := buyAccount.AssetInfo[e.buyOfferAssetId].OfferCanceledOrFinalized
	buyOffer = new(big.Int).SetBit(buyOffer, int(e.buyOfferIndex), 1)
	buyAccount.AssetInfo[e.buyOfferAssetId].OfferCanceledOrFinalized = buyOffer

	// Change new owner.
	matchNft.OwnerAccountIndex = e.buyOffer.AccountIndex

	stateCache := e.bc.StateDB()
	stateCache.SetPendingAccount(fromAccount.AccountIndex, fromAccount)
	stateCache.SetPendingAccount(buyAccount.AccountIndex, buyAccount)
	stateCache.SetPendingAccount(sellAccount.AccountIndex, sellAccount)
	stateCache.SetPendingAccount(creatorAccount.AccountIndex, creatorAccount)
	stateCache.SetPendingNft(matchNft.NftIndex, matchNft)
	stateCache.SetPendingUpdateGas(e.buyOffer.AssetId, txInfo.TreasuryAmount)
	stateCache.SetPendingUpdateGas(txInfo.GasFeeAssetId, txInfo.GasFeeAssetAmount)
	return e.BaseExecutor.ApplyTransaction()
}

func (e *AtomicMatchExecutor) GeneratePubData() error {
	txInfo := e.txInfo

	var buf bytes.Buffer
	buf.WriteByte(uint8(types.TxTypeAtomicMatch))
	buf.Write(common2.Uint32ToBytes(uint32(txInfo.AccountIndex)))
	buf.Write(common2.Uint32ToBytes(uint32(e.buyOffer.AccountIndex)))
	buf.Write(common2.Uint24ToBytes(e.buyOffer.OfferId))
	buf.Write(common2.Uint32ToBytes(uint32(e.sellOffer.AccountIndex)))
	buf.Write(common2.Uint24ToBytes(e.sellOffer.OfferId))
	buf.Write(common2.Uint40ToBytes(e.buyOffer.NftIndex))
	buf.Write(common2.Uint16ToBytes(uint16(e.sellOffer.AssetId)))
	chunk1 := common2.SuffixPaddingBufToChunkSize(buf.Bytes())
	buf.Reset()
	packedAmountBytes, err := common2.AmountToPackedAmountBytes(e.buyOffer.AssetAmount)
	if err != nil {
		logx.Errorf("unable to convert amount to packed amount: %s", err.Error())
		return err
	}
	buf.Write(packedAmountBytes)
	creatorAmountBytes, err := common2.AmountToPackedAmountBytes(txInfo.CreatorAmount)
	if err != nil {
		logx.Errorf("unable to convert amount to packed amount: %s", err.Error())
		return err
	}
	buf.Write(creatorAmountBytes)
	treasuryAmountBytes, err := common2.AmountToPackedAmountBytes(txInfo.TreasuryAmount)
	if err != nil {
		logx.Errorf("unable to convert amount to packed amount: %s", err.Error())
		return err
	}
	buf.Write(treasuryAmountBytes)
	buf.Write(common2.Uint32ToBytes(uint32(txInfo.GasAccountIndex)))
	buf.Write(common2.Uint16ToBytes(uint16(txInfo.GasFeeAssetId)))
	packedFeeBytes, err := common2.FeeToPackedFeeBytes(txInfo.GasFeeAssetAmount)
	if err != nil {
		logx.Errorf("unable to convert amount to packed fee amount: %s", err.Error())
		return err
	}
	buf.Write(packedFeeBytes)
	chunk2 := common2.PrefixPaddingBufToChunkSize(buf.Bytes())
	buf.Reset()
	buf.Write(chunk1)
	buf.Write(chunk2)
	buf.Write(common2.PrefixPaddingBufToChunkSize([]byte{}))
	buf.Write(common2.PrefixPaddingBufToChunkSize([]byte{}))
	buf.Write(common2.PrefixPaddingBufToChunkSize([]byte{}))
	buf.Write(common2.PrefixPaddingBufToChunkSize([]byte{}))
	pubData := buf.Bytes()

	stateCache := e.bc.StateDB()
	stateCache.PubData = append(stateCache.PubData, pubData...)
	return nil
}

func (e *AtomicMatchExecutor) GetExecutedTx() (*tx.Tx, error) {
	txInfoBytes, err := json.Marshal(e.txInfo)
	if err != nil {
		logx.Errorf("unable to marshal tx, err: %s", err.Error())
		return nil, errors.New("unmarshal tx failed")
	}

	e.tx.TxInfo = string(txInfoBytes)
	e.tx.GasFeeAssetId = e.txInfo.GasFeeAssetId
	e.tx.GasFee = e.txInfo.GasFeeAssetAmount.String()
	e.tx.NftIndex = e.txInfo.Offer.NftIndex
	e.tx.AssetId = e.txInfo.Offer.AssetId
	e.tx.TxAmount = e.txInfo.Offer.AssetAmount.String()
	return e.BaseExecutor.GetExecutedTx()
}

func (e *AtomicMatchExecutor) GenerateTxDetails() ([]*tx.TxDetail, error) {
	bc := e.bc
	txInfo := e.txInfo
	matchNft, err := bc.StateDB().GetNft(e.sellOffer.NftIndex)
	if err != nil {
		return nil, err
	}

	copiedAccounts, err := e.bc.StateDB().DeepCopyAccounts([]int64{txInfo.AccountIndex, txInfo.GasAccountIndex,
		e.sellOffer.AccountIndex, e.buyOffer.AccountIndex, matchNft.CreatorAccountIndex})
	if err != nil {
		return nil, err
	}
	fromAccount := copiedAccounts[txInfo.AccountIndex]
	buyAccount := copiedAccounts[e.buyOffer.AccountIndex]
	sellAccount := copiedAccounts[e.sellOffer.AccountIndex]
	creatorAccount := copiedAccounts[matchNft.CreatorAccountIndex]
	gasAccount := copiedAccounts[txInfo.GasAccountIndex]

	txDetails := make([]*tx.TxDetail, 0, 9)

	order := int64(0)
	accountOrder := int64(0)

	fromAccountTxDetail := &tx.TxDetail{
		AssetId:      txInfo.GasFeeAssetId,
		AssetType:    types.FungibleAssetType,
		AccountIndex: txInfo.AccountIndex,
		AccountName:  fromAccount.AccountName,
		Balance:      fromAccount.AssetInfo[txInfo.GasFeeAssetId].String(),
		BalanceDelta: types.ConstructAccountAsset(
			txInfo.GasFeeAssetId, ffmath.Neg(txInfo.GasFeeAssetAmount), types.ZeroBigInt).String(),
		Order:           order,
		AccountOrder:    accountOrder,
		Nonce:           fromAccount.Nonce,
		CollectionNonce: fromAccount.CollectionNonce,
	}

	// buyer asset A
	order++
	accountOrder++
	txDetails = append(txDetails, &tx.TxDetail{
		AssetId:      e.buyOffer.AssetId,
		AssetType:    types.FungibleAssetType,
		AccountIndex: e.buyOffer.AccountIndex,
		AccountName:  buyAccount.AccountName,
		Balance:      buyAccount.AssetInfo[e.buyOffer.AssetId].String(),
		BalanceDelta: types.ConstructAccountAsset(
			e.buyOffer.AssetId, ffmath.Neg(e.buyOffer.AssetAmount), types.ZeroBigInt,
		).String(),
		Order:           order,
		AccountOrder:    accountOrder,
		Nonce:           buyAccount.Nonce,
		CollectionNonce: buyAccount.CollectionNonce,
	})
	buyAccount.AssetInfo[e.buyOffer.AssetId].Balance = ffmath.Sub(buyAccount.AssetInfo[e.buyOffer.AssetId].Balance, e.buyOffer.AssetAmount)

	// buy offer
	order++
	buyOffer := buyAccount.AssetInfo[e.buyOfferAssetId].OfferCanceledOrFinalized
	buyOffer = new(big.Int).SetBit(buyOffer, int(e.buyOfferIndex), 1)
	txDetails = append(txDetails, &tx.TxDetail{
		AssetId:      e.buyOfferAssetId,
		AssetType:    types.FungibleAssetType,
		AccountIndex: e.buyOffer.AccountIndex,
		AccountName:  buyAccount.AccountName,
		Balance:      buyAccount.AssetInfo[e.buyOfferAssetId].String(),
		BalanceDelta: types.ConstructAccountAsset(
			e.buyOfferAssetId, types.ZeroBigInt, buyOffer).String(),
		Order:           order,
		AccountOrder:    accountOrder,
		Nonce:           buyAccount.Nonce,
		CollectionNonce: buyAccount.CollectionNonce,
	})
	buyAccount.AssetInfo[e.buyOfferAssetId].OfferCanceledOrFinalized = buyOffer

	if fromAccount.AccountIndex == buyAccount.AccountIndex {
		order++
		txDetails = append(txDetails, fromAccountTxDetail)
		fromAccount.AssetInfo[txInfo.GasFeeAssetId].Balance = ffmath.Sub(fromAccount.AssetInfo[txInfo.GasFeeAssetId].Balance, txInfo.GasFeeAssetAmount)
	}

	// seller asset A
	order++
	accountOrder++
	sellDeltaAmount := ffmath.Sub(e.sellOffer.AssetAmount, ffmath.Add(txInfo.TreasuryAmount, txInfo.CreatorAmount))
	txDetails = append(txDetails, &tx.TxDetail{
		AssetId:      e.sellOffer.AssetId,
		AssetType:    types.FungibleAssetType,
		AccountIndex: e.sellOffer.AccountIndex,
		AccountName:  sellAccount.AccountName,
		Balance:      sellAccount.AssetInfo[e.sellOffer.AssetId].String(),
		BalanceDelta: types.ConstructAccountAsset(
			e.sellOffer.AssetId, sellDeltaAmount, types.ZeroBigInt,
		).String(),
		Order:           order,
		AccountOrder:    accountOrder,
		Nonce:           sellAccount.Nonce,
		CollectionNonce: sellAccount.CollectionNonce,
	})
	sellAccount.AssetInfo[e.sellOffer.AssetId].Balance = ffmath.Add(sellAccount.AssetInfo[e.sellOffer.AssetId].Balance, sellDeltaAmount)

	// sell offer
	order++
	sellOffer := sellAccount.AssetInfo[e.sellOfferAssetId].OfferCanceledOrFinalized
	sellOffer = new(big.Int).SetBit(sellOffer, int(e.sellOfferIndex), 1)
	txDetails = append(txDetails, &tx.TxDetail{
		AssetId:      e.sellOfferAssetId,
		AssetType:    types.FungibleAssetType,
		AccountIndex: e.sellOffer.AccountIndex,
		AccountName:  sellAccount.AccountName,
		Balance:      sellAccount.AssetInfo[e.sellOfferAssetId].String(),
		BalanceDelta: types.ConstructAccountAsset(
			e.sellOfferAssetId, types.ZeroBigInt, sellOffer).String(),
		Order:           order,
		AccountOrder:    accountOrder,
		Nonce:           sellAccount.Nonce,
		CollectionNonce: sellAccount.CollectionNonce,
	})
	sellAccount.AssetInfo[e.sellOfferAssetId].OfferCanceledOrFinalized = sellOffer

	if fromAccount.AccountIndex == buyAccount.AccountIndex {
		order++
		txDetails = append(txDetails, fromAccountTxDetail)
		fromAccount.AssetInfo[txInfo.GasFeeAssetId].Balance = ffmath.Sub(fromAccount.AssetInfo[txInfo.GasFeeAssetId].Balance, txInfo.GasFeeAssetAmount)
	}

	// creator fee
	order++
	accountOrder++
	txDetails = append(txDetails, &tx.TxDetail{
		AssetId:      e.buyOffer.AssetId,
		AssetType:    types.FungibleAssetType,
		AccountIndex: matchNft.CreatorAccountIndex,
		AccountName:  creatorAccount.AccountName,
		Balance:      creatorAccount.AssetInfo[e.buyOffer.AssetId].String(),
		BalanceDelta: types.ConstructAccountAsset(
			e.buyOffer.AssetId, txInfo.CreatorAmount, types.ZeroBigInt,
		).String(),
		Order:           order,
		AccountOrder:    accountOrder,
		Nonce:           creatorAccount.Nonce,
		CollectionNonce: creatorAccount.CollectionNonce,
	})
	creatorAccount.AssetInfo[e.buyOffer.AssetId].Balance = ffmath.Add(creatorAccount.AssetInfo[e.buyOffer.AssetId].Balance, txInfo.CreatorAmount)

	// nft info
	order++
	txDetails = append(txDetails, &tx.TxDetail{
		AssetId:      matchNft.NftIndex,
		AssetType:    types.NftAssetType,
		AccountIndex: types.NilAccountIndex,
		AccountName:  types.NilAccountName,
		Balance: types.ConstructNftInfo(matchNft.NftIndex, matchNft.CreatorAccountIndex, matchNft.OwnerAccountIndex,
			matchNft.NftContentHash, matchNft.NftL1TokenId, matchNft.NftL1Address, matchNft.CreatorTreasuryRate, matchNft.CollectionId).String(),
		BalanceDelta: types.ConstructNftInfo(matchNft.NftIndex, matchNft.CreatorAccountIndex, e.buyOffer.AccountIndex,
			matchNft.NftContentHash, matchNft.NftL1TokenId, matchNft.NftL1Address, matchNft.CreatorTreasuryRate, matchNft.CollectionId).String(),
		Order:           order,
		AccountOrder:    types.NilAccountOrder,
		Nonce:           0,
		CollectionNonce: 0,
	})

	// gas account asset A - treasury fee
	order++
	accountOrder++
	txDetails = append(txDetails, &tx.TxDetail{
		AssetId:      e.buyOffer.AssetId,
		AssetType:    types.FungibleAssetType,
		AccountIndex: txInfo.GasAccountIndex,
		AccountName:  gasAccount.AccountName,
		Balance:      gasAccount.AssetInfo[e.buyOffer.AssetId].String(),
		BalanceDelta: types.ConstructAccountAsset(
			e.buyOffer.AssetId, txInfo.TreasuryAmount, types.ZeroBigInt).String(),
		Order:           order,
		AccountOrder:    accountOrder,
		Nonce:           gasAccount.Nonce,
		CollectionNonce: gasAccount.CollectionNonce,
		IsGas:           true,
	})
	gasAccount.AssetInfo[e.buyOffer.AssetId].Balance = ffmath.Add(gasAccount.AssetInfo[e.buyOffer.AssetId].Balance, txInfo.TreasuryAmount)

	// gas account asset gas
	order++
	txDetails = append(txDetails, &tx.TxDetail{
		AssetId:      txInfo.GasFeeAssetId,
		AssetType:    types.FungibleAssetType,
		AccountIndex: txInfo.GasAccountIndex,
		AccountName:  gasAccount.AccountName,
		Balance:      gasAccount.AssetInfo[txInfo.GasFeeAssetId].String(),
		BalanceDelta: types.ConstructAccountAsset(
			txInfo.GasFeeAssetId, txInfo.GasFeeAssetAmount, types.ZeroBigInt).String(),
		Order:           order,
		AccountOrder:    accountOrder,
		Nonce:           gasAccount.Nonce,
		CollectionNonce: gasAccount.CollectionNonce,
		IsGas:           true,
	})
	gasAccount.AssetInfo[txInfo.GasFeeAssetId].Balance = ffmath.Add(gasAccount.AssetInfo[txInfo.GasFeeAssetId].Balance, txInfo.GasFeeAssetAmount)

	return txDetails, nil
}
