/*
 * Copyright © 2021 ZkBNB Protocol
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package sender

import (
	"encoding/json"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/zeromicro/go-zero/core/logx"

	zkbnb "github.com/bnb-chain/zkbnb-eth-rpc/zkbnb/core/legend"
	"github.com/bnb-chain/zkbnb/common/chain"
	"github.com/bnb-chain/zkbnb/dao/block"
	"github.com/bnb-chain/zkbnb/dao/compressedblock"
	"github.com/bnb-chain/zkbnb/tree"
	"github.com/bnb-chain/zkbnb/types"
)

const (
	EventNameBlockCommit       = "BlockCommit"
	EventNameBlockVerification = "BlockVerification"
)

var (
	ZkBNBContractAbi, _ = abi.JSON(strings.NewReader(zkbnb.ZkBNBMetaData.ABI))

	zkbnbLogBlockCommitSig       = []byte("BlockCommit(uint32)")
	zkbnbLogBlockVerificationSig = []byte("BlockVerification(uint32)")
	zkbnbLogBlocksRevertSig      = []byte("BlocksRevert(uint32,uint32)")

	zkbnbLogBlockCommitSigHash       = crypto.Keccak256Hash(zkbnbLogBlockCommitSig)
	zkbnbLogBlockVerificationSigHash = crypto.Keccak256Hash(zkbnbLogBlockVerificationSig)
	zkbnbLogBlocksRevertSigHash      = crypto.Keccak256Hash(zkbnbLogBlocksRevertSig)
)

func defaultBlockHeader() zkbnb.StorageStoredBlockInfo {
	var (
		pendingOnChainOperationsHash [32]byte
		stateRoot                    [32]byte
		commitment                   [32]byte
	)
	copy(pendingOnChainOperationsHash[:], common.FromHex(types.EmptyStringKeccak)[:])
	copy(stateRoot[:], tree.NilStateRoot[:])
	copy(commitment[:], common.FromHex("0x0000000000000000000000000000000000000000000000000000000000000000")[:])
	return zkbnb.StorageStoredBlockInfo{
		BlockSize:                    0,
		BlockNumber:                  0,
		PriorityOperations:           0,
		PendingOnchainOperationsHash: pendingOnChainOperationsHash,
		Timestamp:                    big.NewInt(0),
		StateRoot:                    stateRoot,
		Commitment:                   commitment,
	}
}

func ConvertBlocksForCommitToCommitBlockInfos(oBlocks []*compressedblock.CompressedBlock) (commitBlocks []zkbnb.OldZkBNBCommitBlockInfo, err error) {
	for _, oBlock := range oBlocks {
		var newStateRoot [32]byte
		var pubDataOffsets []uint32
		copy(newStateRoot[:], common.FromHex(oBlock.StateRoot)[:])
		err = json.Unmarshal([]byte(oBlock.PublicDataOffsets), &pubDataOffsets)
		if err != nil {
			logx.Errorf("[ConvertBlocksForCommitToCommitBlockInfos] unable to unmarshal: %s", err.Error())
			return nil, err
		}
		commitBlock := zkbnb.OldZkBNBCommitBlockInfo{
			NewStateRoot:      newStateRoot,
			PublicData:        common.FromHex(oBlock.PublicData),
			Timestamp:         big.NewInt(oBlock.Timestamp),
			PublicDataOffsets: pubDataOffsets,
			BlockNumber:       uint32(oBlock.BlockHeight),
			BlockSize:         oBlock.BlockSize,
		}
		commitBlocks = append(commitBlocks, commitBlock)
	}
	return commitBlocks, nil
}

func ConvertBlocksToVerifyAndExecuteBlockInfos(oBlocks []*block.Block) (verifyAndExecuteBlocks []zkbnb.OldZkBNBVerifyAndExecuteBlockInfo, err error) {
	for _, oBlock := range oBlocks {
		var pendingOnChainOpsPubData [][]byte
		if oBlock.PendingOnChainOperationsPubData != "" {
			err = json.Unmarshal([]byte(oBlock.PendingOnChainOperationsPubData), &pendingOnChainOpsPubData)
			if err != nil {
				logx.Errorf("[ConvertBlocksToVerifyAndExecuteBlockInfos] unable to unmarshal pending pub data: %s", err.Error())
				return nil, err
			}
		}
		verifyAndExecuteBlock := zkbnb.OldZkBNBVerifyAndExecuteBlockInfo{
			BlockHeader:              chain.ConstructStoredBlockInfo(oBlock),
			PendingOnchainOpsPubData: pendingOnChainOpsPubData,
		}
		verifyAndExecuteBlocks = append(verifyAndExecuteBlocks, verifyAndExecuteBlock)
	}
	return verifyAndExecuteBlocks, nil
}
