package witness

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/panjf2000/ants/v2"
	"github.com/zeromicro/go-zero/core/logx"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	cryptoBlock "github.com/bnb-chain/zkbnb-crypto/legend/circuit/bn254/block"
	smt "github.com/bnb-chain/zkbnb-smt"
	utils "github.com/bnb-chain/zkbnb/common/prove"
	"github.com/bnb-chain/zkbnb/dao/account"
	"github.com/bnb-chain/zkbnb/dao/block"
	"github.com/bnb-chain/zkbnb/dao/blockwitness"
	"github.com/bnb-chain/zkbnb/dao/liquidity"
	"github.com/bnb-chain/zkbnb/dao/nft"
	"github.com/bnb-chain/zkbnb/dao/proof"
	"github.com/bnb-chain/zkbnb/service/witness/config"
	"github.com/bnb-chain/zkbnb/tree"
	"github.com/bnb-chain/zkbnb/types"
)

const (
	UnprovedBlockWitnessTimeout = 10 * time.Minute

	BlockProcessDelta = 10

	defaultTaskPoolSize = 1000
)

type Witness struct {
	// config
	config config.Config
	helper *utils.WitnessHelper

	// Trees
	treeCtx       *tree.Context
	accountTree   smt.SparseMerkleTree
	assetTrees    []smt.SparseMerkleTree
	liquidityTree smt.SparseMerkleTree
	nftTree       smt.SparseMerkleTree
	taskPool      *ants.Pool

	// The data access object
	blockModel            block.BlockModel
	accountModel          account.AccountModel
	accountHistoryModel   account.AccountHistoryModel
	liquidityHistoryModel liquidity.LiquidityHistoryModel
	nftHistoryModel       nft.L2NftHistoryModel
	proofModel            proof.ProofModel
	blockWitnessModel     blockwitness.BlockWitnessModel
}

func NewWitness(c config.Config) (*Witness, error) {
	datasource := c.Postgres.DataSource
	db, err := gorm.Open(postgres.Open(datasource))
	if err != nil {
		return nil, fmt.Errorf("gorm connect db error, err: %v", err)
	}

	w := &Witness{
		config:                c,
		blockModel:            block.NewBlockModel(db),
		blockWitnessModel:     blockwitness.NewBlockWitnessModel(db),
		accountModel:          account.NewAccountModel(db),
		accountHistoryModel:   account.NewAccountHistoryModel(db),
		liquidityHistoryModel: liquidity.NewLiquidityHistoryModel(db),
		nftHistoryModel:       nft.NewL2NftHistoryModel(db),
		proofModel:            proof.NewProofModel(db),
	}
	err = w.initState()
	return w, err
}

func (w *Witness) initState() error {
	witnessHeight, err := w.blockWitnessModel.GetLatestBlockWitnessHeight()
	if err != nil {
		if err != types.DbErrNotFound {
			return fmt.Errorf("GetLatestBlockWitness error: %v", err)
		}

		witnessHeight = 0
	}

	// dbinitializer tree database
	treeCtx := &tree.Context{
		Name:          "witness",
		Driver:        w.config.TreeDB.Driver,
		LevelDBOption: &w.config.TreeDB.LevelDBOption,
		RedisDBOption: &w.config.TreeDB.RedisDBOption,
	}
	err = tree.SetupTreeDB(treeCtx)
	if err != nil {
		return fmt.Errorf("init tree database failed %v", err)
	}
	w.treeCtx = treeCtx

	// dbinitializer accountTree and accountStateTrees
	// the dbinitializer block number use the latest sent block
	w.accountTree, w.assetTrees, err = tree.InitAccountTree(
		w.accountModel,
		w.accountHistoryModel,
		witnessHeight,
		treeCtx,
	)
	// the blockHeight depends on the proof start position
	if err != nil {
		return fmt.Errorf("initMerkleTree error: %v", err)
	}

	w.liquidityTree, err = tree.InitLiquidityTree(w.liquidityHistoryModel, witnessHeight,
		treeCtx)
	if err != nil {
		return fmt.Errorf("initLiquidityTree error: %v", err)
	}
	w.nftTree, err = tree.InitNftTree(w.nftHistoryModel, witnessHeight,
		treeCtx)
	if err != nil {
		return fmt.Errorf("initNftTree error: %v", err)
	}
	taskPool, err := ants.NewPool(defaultTaskPoolSize)
	if err != nil {
		return err
	}
	w.taskPool = taskPool
	w.helper = utils.NewWitnessHelper(w.treeCtx, w.accountTree, w.liquidityTree, w.nftTree, &w.assetTrees, w.accountModel)
	return nil
}

func (w *Witness) GenerateBlockWitness() (err error) {
	var latestWitnessHeight int64
	latestWitnessHeight, err = w.blockWitnessModel.GetLatestBlockWitnessHeight()
	if err != nil && err != types.DbErrNotFound {
		return err
	}
	// get next batch of blocks
	blocks, err := w.blockModel.GetBlocksBetween(latestWitnessHeight+1, latestWitnessHeight+BlockProcessDelta)
	if err != nil {
		if err != types.DbErrNotFound {
			return err
		}
		return nil
	}
	// get latestVerifiedBlockNr
	latestVerifiedBlockNr, err := w.blockModel.GetLatestVerifiedHeight()
	if err != nil {
		return err
	}

	// scan each block
	for _, block := range blocks {
		// Step1: construct witness
		blockWitness, err := w.constructBlockWitness(block, latestVerifiedBlockNr)
		if err != nil {
			return fmt.Errorf("failed to construct block witness, err: %v", err)
		}
		// Step2: commit trees for witness
		err = tree.CommitTrees(w.taskPool, uint64(latestVerifiedBlockNr), w.accountTree, &w.assetTrees, w.liquidityTree, w.nftTree)
		if err != nil {
			return fmt.Errorf("unable to commit trees after txs is executed, error: %v", err)
		}
		// Step3: insert witness into database
		err = w.blockWitnessModel.CreateBlockWitness(blockWitness)
		if err != nil {
			// rollback trees
			rollBackErr := tree.RollBackTrees(w.taskPool, uint64(block.BlockHeight)-1, w.accountTree, &w.assetTrees, w.liquidityTree, w.nftTree)
			if rollBackErr != nil {
				logx.Errorf("unable to rollback trees %v", rollBackErr)
			}
			return fmt.Errorf("create unproved crypto block error, err: %v", err)
		}
	}
	return nil
}

func (w *Witness) RescheduleBlockWitness() {
	nextBlockNumber, err := w.getNextWitnessToCheck()
	if err != nil {
		logx.Errorf("failed to get next witness to check, err: %s", err.Error())
	}
	nextBlockWitness, err := w.blockWitnessModel.GetBlockWitnessByHeight(nextBlockNumber)
	if err != nil {
		logx.Errorf("failed to get latest block witness, err: %s", err.Error())
		return
	}

	// skip if next block is not processed
	if nextBlockWitness.Status == blockwitness.StatusPublished {
		return
	}

	// skip if the next block proof exists
	// if the proof is not submitted and verified in L1, there should be another alerts
	_, err = w.proofModel.GetProofByBlockHeight(nextBlockNumber)
	if err == nil {
		return
	}

	// update block status to Published if it's timeout
	if time.Now().After(nextBlockWitness.UpdatedAt.Add(UnprovedBlockWitnessTimeout)) {
		err := w.blockWitnessModel.UpdateBlockWitnessStatus(nextBlockWitness, blockwitness.StatusPublished)
		if err != nil {
			logx.Errorf("update unproved block status error, err: %s", err.Error())
			return
		}
	}
}

func (w *Witness) getNextWitnessToCheck() (int64, error) {
	latestProof, err := w.proofModel.GetLatestProof()
	if err != nil && err != types.DbErrNotFound {
		logx.Errorf("failed to get latest proof, err: %s", err.Error())
		return 0, err
	}

	if err == types.DbErrNotFound {
		return 1, nil
	}

	latestConfirmedProof, err := w.proofModel.GetLatestConfirmedProof()
	if err != nil && err != types.DbErrNotFound {
		logx.Errorf("failed to get latest confirmed proof, err: %s", err.Error())
		return 0, err
	}

	var startToCheck, endToCheck int64 = 1, latestProof.BlockNumber
	if err != types.DbErrNotFound {
		startToCheck = latestConfirmedProof.BlockNumber + 1
	}

	for blockHeight := startToCheck; blockHeight < endToCheck; blockHeight++ {
		_, err = w.proofModel.GetProofByBlockHeight(blockHeight)
		if err != nil {
			return blockHeight, nil
		}
	}
	return endToCheck + 1, nil
}

func (w *Witness) constructBlockWitness(block *block.Block, latestVerifiedBlockNr int64) (*blockwitness.BlockWitness, error) {
	var oldStateRoot, newStateRoot []byte
	txsWitness := make([]*utils.TxWitness, 0, block.BlockSize)
	// scan each transaction
	for idx, tx := range block.Txs {
		txWitness, err := w.helper.ConstructTxWitness(tx, uint64(latestVerifiedBlockNr))
		if err != nil {
			return nil, err
		}
		txsWitness = append(txsWitness, txWitness)
		// if it is the first tx of the block
		if idx == 0 {
			oldStateRoot = txWitness.StateRootBefore
		}
		// if it is the last tx of the block
		if idx == len(block.Txs)-1 {
			newStateRoot = txWitness.StateRootAfter
		}
	}

	emptyTxCount := int(block.BlockSize) - len(block.Txs)
	for i := 0; i < emptyTxCount; i++ {
		txsWitness = append(txsWitness, cryptoBlock.EmptyTx())
	}
	if common.Bytes2Hex(newStateRoot) != block.StateRoot {
		return nil, errors.New("state root doesn't match")
	}

	b := &cryptoBlock.Block{
		BlockNumber:     block.BlockHeight,
		CreatedAt:       block.CreatedAt.UnixMilli(),
		OldStateRoot:    oldStateRoot,
		NewStateRoot:    newStateRoot,
		BlockCommitment: common.FromHex(block.BlockCommitment),
		Txs:             txsWitness,
	}
	bz, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	blockWitness := blockwitness.BlockWitness{
		Height:      block.BlockHeight,
		WitnessData: string(bz),
		Status:      blockwitness.StatusPublished,
	}
	return &blockWitness, nil
}
