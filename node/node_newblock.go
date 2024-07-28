package node

import (
	"errors"
	"math/big"
	"math/rand"
	"sort"
	"strings"
	"time"

	blockif "github.com/harmony-one/harmony/block/interface"
	"github.com/harmony-one/harmony/consensus"

	"github.com/harmony-one/harmony/crypto/bls"

	staking "github.com/harmony-one/harmony/staking/types"

	"github.com/ethereum/go-ethereum/common"
	"github.com/harmony-one/go-sdk/pkg/store"
	"github.com/harmony-one/harmony/core/rawdb"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/shard"
)

// Constants of proposing a new block
const (
	SleepPeriod           = 20 * time.Millisecond
	IncomingReceiptsLimit = 60000 // 2000 * (numShards - 1)
)

// WaitForConsensusReadyV2 listen for the readiness signal from consensus and generate new block for consensus.
// only leader will receive the ready signal
func (node *Node) WaitForConsensusReadyV2(readySignal chan consensus.ProposalType, commitSigsChan chan []byte, stopChan chan struct{}, stoppedChan chan struct{}) {
	go func() {
		// Setup stoppedChan
		defer close(stoppedChan)

		utils.Logger().Debug().
			Msg("Waiting for Consensus ready")
		select {
		case <-time.After(30 * time.Second):
		case <-stopChan:
			return
		}

		for {
			// keep waiting for Consensus ready
			select {
			case <-stopChan:
				utils.Logger().Warn().
					Msg("Consensus new block proposal: STOPPED!")
				return
			case proposalType := <-readySignal:
				retryCount := 0
				for node.Consensus != nil && node.Consensus.IsLeader() {
					time.Sleep(SleepPeriod)
					utils.Logger().Info().
						Uint64("blockNum", node.Blockchain().CurrentBlock().NumberU64()+1).
						Bool("asyncProposal", proposalType == consensus.AsyncProposal).
						Msg("PROPOSING NEW BLOCK ------------------------------------------------")

					// Prepare last commit signatures
					newCommitSigsChan := make(chan []byte)

					go func() {
						waitTime := 0 * time.Second
						if proposalType == consensus.AsyncProposal {
							waitTime = consensus.CommitSigReceiverTimeout
						}
						select {
						case <-time.After(waitTime):
							if waitTime == 0 {
								utils.Logger().Info().Msg("[ProposeNewBlock] Sync block proposal, reading commit sigs directly from DB")
							} else {
								utils.Logger().Info().Msg("[ProposeNewBlock] Timeout waiting for commit sigs, reading directly from DB")
							}
							sigs, err := node.Consensus.BlockCommitSigs(node.Blockchain().CurrentBlock().NumberU64())

							if err != nil {
								utils.Logger().Error().Err(err).Msg("[ProposeNewBlock] Cannot get commit signatures from last block")
							} else {
								newCommitSigsChan <- sigs
							}
						case commitSigs := <-commitSigsChan:
							utils.Logger().Info().Msg("[ProposeNewBlock] received commit sigs asynchronously")
							if len(commitSigs) > bls.BLSSignatureSizeInBytes {
								newCommitSigsChan <- commitSigs
							}
						}
					}()
					newBlock, err := node.ProposeNewBlock(newCommitSigsChan)
					// uhhBot do something to update the transaction pool
					// 交易的产生不能依赖于新区块了
					// 暂时先不要产生交易
					// node.ProposeRawTransactionsTmp()
					if err == nil {
						if blk, ok := node.proposedBlock[newBlock.NumberU64()]; ok {
							utils.Logger().Info().Uint64("blockNum", newBlock.NumberU64()).Str("blockHash", blk.Hash().Hex()).
								Msg("Block with the same number was already proposed, abort.")
							break
						}
						utils.Logger().Info().
							Uint64("blockNum", newBlock.NumberU64()).
							Uint64("epoch", newBlock.Epoch().Uint64()).
							Uint64("viewID", newBlock.Header().ViewID().Uint64()).
							Int("numTxs", newBlock.Transactions().Len()).
							Int("numStakingTxs", newBlock.StakingTransactions().Len()).
							Int("crossShardReceipts", newBlock.IncomingReceipts().Len()).
							Msg("=========Successfully Proposed New Block==========")

						// Send the new block to Consensus so it can be confirmed.
						node.proposedBlock[newBlock.NumberU64()] = newBlock
						delete(node.proposedBlock, newBlock.NumberU64()-10)
						node.BlockChannel <- newBlock
						break
					} else {
						retryCount++
						utils.Logger().Err(err).Int("retryCount", retryCount).
							Msg("!!!!!!!!!Failed Proposing New Block!!!!!!!!!")
						if retryCount > 3 {
							// break to avoid repeated failures
							break
						}
						continue
					}
				}
			}
		}
	}()
}

// uhhBot
// 直接往里面加交易
// ProposeRawTransactions proposes a batch of transactions to the peers
func (node *Node) ProposeRawTransactions() {
	// utils.Logger().Debug().Msg("uhhBot this is leader")
	// TODO 换一个人提出交易

	var curBlockViewID = node.Consensus.GetCurBlockViewID()

	if !node.Consensus.IsLeader() || curBlockViewID != node.Consensus.FirstTransactionTime {
		return
	}
	utils.Logger().Info().Msg("uhhBot only run one time")

	header := node.Worker.GetCurrentHeader()
	leaderKey := node.Consensus.LeaderPubKey
	numShards := int(shard.Schedule.InstanceForEpoch(header.Epoch()).NumShards())

	crossRadio := 0.0
	switch {
	case numShards >= 16:
		crossRadio = 1
	case numShards >= 12:
		crossRadio = 0.92
	case numShards >= 10:
		crossRadio = 0.88
	case numShards >= 8:
		crossRadio = 0.87
	case numShards >= 6:
		crossRadio = 0.84
	case numShards >= 4:
		crossRadio = 0.77
	case numShards >= 2:
		crossRadio = 0.6
	default:
		crossRadio = 0.0
	}

	fromAddr := node.GetAddressForBLSKey(leaderKey.Object, header.Epoch())
	if node.Ks == nil && node.Acct == nil {
		var err error
		node.Ks, node.Acct, err = store.UnlockedKeystore(fromAddr.String(), "")
		if err != nil {
			utils.Logger().Err(err).Str("fromAddr is", fromAddr.String()).Msg("uhhBot oh no fail to unlockkeystore")
		}
	}

	// magic number
	fromShard := node.Consensus.ShardID
	// beacon chain不用干脏活
	// if fromShard == 0 {
	// 	time.Sleep(20 * time.Second)
	// 	return
	// }
	blockTxsCnt := node.Consensus.BlockTxsCnt
	// nonce 需要重新计算
	ticker := time.NewTicker(time.Duration(node.Consensus.WaitTime) * time.Second)

	utils.Logger().Info().Int("node.Consensus.WaitTime", node.Consensus.WaitTime).Msg("uhhBot")
	lastNonce := 0
	// 增加的速度会随着时间改变的 每次产块多10个区块
	addingNum := 32
	go func() {
		for {
			select {
			case <-ticker.C:
				for Index := 0; Index < addingNum; Index++ {
					// cross shard
					toShard := fromShard
					if Index < int(crossRadio*float64(addingNum)) {
						toShard = uint32(rand.Intn(numShards))
						if toShard == fromShard {
							toShard = (fromShard + 1) % uint32(numShards)
							if toShard == 0 {
								toShard = (toShard + 1) % uint32(numShards)
							}
						}
					}
					payload := make([]byte, 2*1024*1024/blockTxsCnt)
					rawTx := types.NewCrossShardTransaction(uint64(lastNonce+Index), &fromAddr, fromShard, toShard, common.Big1, uint64(300000), big.NewInt(1000000000), payload) //FIXME: hardcoded shardID 0

					signedTx, err2 := node.Ks.SignTx(*node.Acct, rawTx, big.NewInt(2))

					if err2 != nil {
						utils.Logger().Err(err2).Msg("uhhBot oh no fail to sign new tx")
					}

					txs := types.Transactions{}
					txs = append(txs, signedTx)
					node.AddPendingTransaction(txs[0])
					// node.addPendingTransactions(txs)
				}
				lastNonce += addingNum
				// addingNum += 10
			}
			if lastNonce >= 32*6 {
				break
			}
		}
	}()
	// for nonce := 0; nonce < blockTxsCnt; nonce++ {
	// 	// cross shard
	// 	toShard := fromShard
	// 	if nonce < int(crossRadio*float64(blockTxsCnt)) {
	// 		toShard = uint32(rand.Intn(numShards))
	// 		if toShard == fromShard {
	// 			toShard = (fromShard + 1) % uint32(numShards)
	// 			if toShard == 0 {
	// 				toShard = (toShard + 1) % uint32(numShards)
	// 			}
	// 		}
	// 	}
	// 	payload := make([]byte, 2*1024*1024/blockTxsCnt)
	// 	realNonce := uint64(nonce) + uint64(curBlockViewID-node.Consensus.FirstTransactionTime)*uint64(blockTxsCnt)
	// 	rawTx := types.NewCrossShardTransaction(realNonce, &fromAddr, fromShard, toShard, common.Big1, uint64(300000), big.NewInt(1000000000), payload) //FIXME: hardcoded shardID 0

	// 	signedTx, err2 := node.Ks.SignTx(*node.Acct, rawTx, big.NewInt(2))

	// 	if err2 != nil {
	// 		utils.Logger().Err(err2).Msg("uhhBot oh no fail to sign new tx")
	// 	}

	// 	txs := types.Transactions{}
	// 	txs = append(txs, signedTx)
	// 	node.AddPendingTransaction(txs[0])
	// 	// node.addPendingTransactions(txs)
	// }
}

func (node *Node) ProposeRawTransactionsTmp() {
	// utils.Logger().Debug().Msg("uhhBot this is leader")
	// TODO 换一个人提出交易

	var curBlockViewID = node.Consensus.GetCurBlockViewID()

	if !node.Consensus.IsLeader() || curBlockViewID != node.Consensus.FirstTransactionTime {
		return
	}
	utils.Logger().Info().Msg("uhhBot only run one time")

	header := node.Worker.GetCurrentHeader()
	leaderKey := node.Consensus.LeaderPubKey

	fromAddr := node.GetAddressForBLSKey(leaderKey.Object, header.Epoch())
	if node.Ks == nil && node.Acct == nil {
		var err error
		node.Ks, node.Acct, err = store.UnlockedKeystore(fromAddr.String(), "")
		if err != nil {
			utils.Logger().Err(err).Str("fromAddr is", fromAddr.String()).Msg("uhhBot oh no fail to unlockkeystore")
		}
	}

	fromShard := node.Consensus.ShardID

	blockTxsCnt := node.Consensus.BlockTxsCnt
	utils.Logger().Info().Int("node.Consensus.WaitTime", node.Consensus.WaitTime).Msg("uhhBot")
	// 尝试把nonce的检查拿掉
	lastNonce := 0
	// 增加的速度会随着时间改变的 每次产块多10个区块
	addingNum := 233
	go func() {
		txs := types.Transactions{}
		for Index := 0; Index < addingNum; Index++ {
			// cross shard
			toShard := uint32(1)
			payload := make([]byte, 2*1024*1024/blockTxsCnt-109)
			rawTx := types.NewCrossShardTransaction(uint64(lastNonce+Index), &fromAddr, fromShard, toShard, common.Big1, uint64(300000), big.NewInt(1000000000), payload) //FIXME: hardcoded shardID 0

			signedTx, err2 := node.Ks.SignTx(*node.Acct, rawTx, big.NewInt(2))

			if err2 != nil {
				utils.Logger().Err(err2).Msg("uhhBot oh no fail to sign new tx")
			}

			txs = append(txs, signedTx)
		}
		node.addPendingTransactions(txs)
	}()

}

// ProposeNewBlock proposes a new block...
func (node *Node) ProposeNewBlock(commitSigs chan []byte) (*types.Block, error) {
	currentHeader := node.Blockchain().CurrentHeader()
	nowEpoch, blockNow := currentHeader.Epoch(), currentHeader.Number()
	utils.AnalysisStart("ProposeNewBlock", nowEpoch, blockNow)
	defer utils.AnalysisEnd("ProposeNewBlock", nowEpoch, blockNow)

	node.Worker.UpdateCurrent()

	header := node.Worker.GetCurrentHeader()
	// Update worker's current header and
	// state data in preparation to propose/process new transactions
	leaderKey := node.Consensus.LeaderPubKey
	var (
		coinbase    = node.GetAddressForBLSKey(leaderKey.Object, header.Epoch())
		beneficiary = coinbase
		err         error
	)

	// After staking, all coinbase will be the address of bls pub key
	if node.Blockchain().Config().IsStaking(header.Epoch()) {
		blsPubKeyBytes := leaderKey.Object.GetAddress()
		coinbase.SetBytes(blsPubKeyBytes[:])
	}

	emptyAddr := common.Address{}
	if coinbase == emptyAddr {
		return nil, errors.New("[ProposeNewBlock] Failed setting coinbase")
	}

	// Must set coinbase here because the operations below depend on it
	header.SetCoinbase(coinbase)

	// Get beneficiary based on coinbase
	// Before staking, coinbase itself is the beneficial
	// After staking, beneficial is the corresponding ECDSA address of the bls key
	beneficiary, err = node.Blockchain().GetECDSAFromCoinbase(header)
	if err != nil {
		return nil, err
	}

	// Add VRF
	if node.Blockchain().Config().IsVRF(header.Epoch()) {
		//generate a new VRF for the current block
		if err := node.Consensus.GenerateVrfAndProof(header); err != nil {
			return nil, err
		}
	}

	// uhhBot Prepare cross shard transaction receipts first
	receiptsList := node.proposeReceiptsProof()
	if len(receiptsList) != 0 {
		if err := node.Worker.CommitReceipts(receiptsList); err != nil {
			return nil, err
		}
	}

	// Prepare normal and staking transactions retrieved from transaction pool
	utils.AnalysisStart("proposeNewBlockChooseFromTxnPool")

	pendingPoolTxs, err := node.TxPool.Pending()
	if err != nil {
		utils.Logger().Err(err).Msg("Failed to fetch pending transactions")
		return nil, err
	}
	pendingPlainTxs := map[common.Address]types.Transactions{}
	pendingStakingTxs := staking.StakingTransactions{}
	index := 0
	//uhhBot maximum number of normal transaction per block
	// maxTxsNumPerBlock := 512 - len(receiptsList)
	// magic number
	blockTxsCnt := node.Consensus.BlockTxsCnt
	for addr, poolTxs := range pendingPoolTxs {

		plainTxsPerAcc := types.Transactions{}
		for _, tx := range poolTxs {
			if index += 1; index > blockTxsCnt {
				break
			}
			if plainTx, ok := tx.(*types.Transaction); ok {
				plainTxsPerAcc = append(plainTxsPerAcc, plainTx)
			} else if stakingTx, ok := tx.(*staking.StakingTransaction); ok {
				// Only process staking transactions after pre-staking epoch happened.
				if node.Blockchain().Config().IsPreStaking(node.Worker.GetCurrentHeader().Epoch()) {
					pendingStakingTxs = append(pendingStakingTxs, stakingTx)
				}
			} else {
				utils.Logger().Err(types.ErrUnknownPoolTxType).
					Msg("Failed to parse pending transactions")
				return nil, types.ErrUnknownPoolTxType
			}
		}
		if plainTxsPerAcc.Len() > 0 {
			pendingPlainTxs[addr] = plainTxsPerAcc
		}
	}
	utils.AnalysisEnd("proposeNewBlockChooseFromTxnPool")

	// Try commit normal and staking transactions based on the current state
	// The successfully committed transactions will be put in the proposed block
	if err := node.Worker.CommitTransactions(
		pendingPlainTxs, pendingStakingTxs, beneficiary,
	); err != nil {
		utils.Logger().Error().Err(err).Msg("cannot commit transactions")
		return nil, err
	}

	isBeaconchainInCrossLinkEra := node.NodeConfig.ShardID == shard.BeaconChainShardID &&
		node.Blockchain().Config().IsCrossLink(node.Worker.GetCurrentHeader().Epoch())

	isBeaconchainInStakingEra := node.NodeConfig.ShardID == shard.BeaconChainShardID &&
		node.Blockchain().Config().IsStaking(node.Worker.GetCurrentHeader().Epoch())

	utils.AnalysisStart("proposeNewBlockVerifyCrossLinks")
	// Prepare cross links and slashing messages
	var crossLinksToPropose types.CrossLinks
	if isBeaconchainInCrossLinkEra {
		allPending, err := node.Blockchain().ReadPendingCrossLinks()
		invalidToDelete := []types.CrossLink{}
		if err == nil {
			for _, pending := range allPending {
				exist, err := node.Blockchain().ReadCrossLink(pending.ShardID(), pending.BlockNum())
				if err == nil || exist != nil {
					invalidToDelete = append(invalidToDelete, pending)
					utils.Logger().Debug().
						AnErr("[ProposeNewBlock] pending crosslink is already committed onchain", err)
					continue
				}

				// Crosslink is already verified before it's accepted to pending,
				// no need to verify again in proposal.
				if !node.Blockchain().Config().IsCrossLink(pending.Epoch()) {
					utils.Logger().Debug().
						AnErr("[ProposeNewBlock] pending crosslink that's before crosslink epoch", err)
					continue
				}

				crossLinksToPropose = append(crossLinksToPropose, pending)
				if len(crossLinksToPropose) > 15 {
					break
				}
			}
			utils.Logger().Info().
				Msgf("[ProposeNewBlock] Proposed %d crosslinks from %d pending crosslinks",
					len(crossLinksToPropose), len(allPending),
				)
		} else {
			// uhhBot clear log
			// utils.Logger().Error().Err(err).Msgf(
			// 	"[ProposeNewBlock] Unable to Read PendingCrossLinks, number of crosslinks: %d",
			// 	len(allPending),
			// )
		}
		node.Blockchain().DeleteFromPendingCrossLinks(invalidToDelete)
	}
	utils.AnalysisEnd("proposeNewBlockVerifyCrossLinks")

	if isBeaconchainInStakingEra {
		// this will set a meaningful w.current.slashes
		if err := node.Worker.CollectVerifiedSlashes(); err != nil {
			return nil, err
		}
	}

	// Prepare shard state
	var shardState *shard.State
	if shardState, err = node.Blockchain().SuperCommitteeForNextEpoch(
		node.Beaconchain(), node.Worker.GetCurrentHeader(), false,
	); err != nil {
		return nil, err
	}

	viewIDFunc := func() uint64 {
		return node.Consensus.GetCurBlockViewID()
	}

	node.Consensus.NewTxnsMutex.Lock()
	// 深拷贝此时的交易
	tmpTXs := append([]blockif.TransactionMsg(nil), node.Consensus.NewTxnsForNextBlock...)
	// 清空交易
	node.Consensus.NewTxnsForNextBlock = make([]blockif.TransactionMsg, 0)
	node.Consensus.NewTxnsMutex.Unlock()

	finalizedBlock, err := node.Worker.FinalizeNewBlock(
		commitSigs, viewIDFunc,
		coinbase, crossLinksToPropose, shardState, node.Consensus.DivisionLevel, node.Consensus.MetaStage[node.Consensus.DivisionLevel].BlockID, tmpTXs,
	)
	if err != nil {
		utils.Logger().Error().Err(err).Msg("[ProposeNewBlock] Failed finalizing the new block")
		return nil, err
	}
	utils.Logger().Info().Msg("[ProposeNewBlock] verifying the new block header")
	err = node.Blockchain().Validator().ValidateHeader(finalizedBlock, true)

	if err != nil {
		utils.Logger().Error().Err(err).Msg("[ProposeNewBlock] Failed verifying the new block header")
		return nil, err
	}
	return finalizedBlock, nil
}

func (node *Node) proposeReceiptsProof() []*types.CXReceiptsProof {
	if !node.Blockchain().Config().HasCrossTxFields(node.Worker.GetCurrentHeader().Epoch()) {
		return []*types.CXReceiptsProof{}
	}

	numProposed := 0
	validReceiptsList := []*types.CXReceiptsProof{}
	pendingReceiptsList := []*types.CXReceiptsProof{}

	node.pendingCXMutex.Lock()
	defer node.pendingCXMutex.Unlock()

	// not necessary to sort the list, but we just prefer to process the list ordered by shard and blocknum
	pendingCXReceipts := []*types.CXReceiptsProof{}
	for _, v := range node.pendingCXReceipts {
		pendingCXReceipts = append(pendingCXReceipts, v)
	}

	sort.SliceStable(pendingCXReceipts, func(i, j int) bool {
		shardCMP := pendingCXReceipts[i].MerkleProof.ShardID < pendingCXReceipts[j].MerkleProof.ShardID
		shardEQ := pendingCXReceipts[i].MerkleProof.ShardID == pendingCXReceipts[j].MerkleProof.ShardID
		blockCMP := pendingCXReceipts[i].MerkleProof.BlockNum.Cmp(
			pendingCXReceipts[j].MerkleProof.BlockNum,
		) == -1
		return shardCMP || (shardEQ && blockCMP)
	})

	m := map[common.Hash]struct{}{}

Loop:
	for _, cxp := range node.pendingCXReceipts {
		if numProposed > IncomingReceiptsLimit {
			pendingReceiptsList = append(pendingReceiptsList, cxp)
			continue
		}
		// check double spent
		if node.Blockchain().IsSpent(cxp) {
			utils.Logger().Debug().Interface("cxp", cxp).Msg("[proposeReceiptsProof] CXReceipt is spent")
			continue
		}
		hash := cxp.MerkleProof.BlockHash
		// ignore duplicated receipts
		if _, ok := m[hash]; ok {
			continue
		} else {
			m[hash] = struct{}{}
		}

		for _, item := range cxp.Receipts {
			if item.ToShardID != node.Blockchain().ShardID() {
				continue Loop
			}
		}

		if err := node.Blockchain().Validator().ValidateCXReceiptsProof(cxp); err != nil {
			if strings.Contains(err.Error(), rawdb.MsgNoShardStateFromDB) {
				pendingReceiptsList = append(pendingReceiptsList, cxp)
			} else {
				utils.Logger().Error().Err(err).Msg("[proposeReceiptsProof] Invalid CXReceiptsProof")
			}
			continue
		}

		utils.Logger().Debug().Interface("cxp", cxp).Msg("[proposeReceiptsProof] CXReceipts Added")
		validReceiptsList = append(validReceiptsList, cxp)
		numProposed = numProposed + len(cxp.Receipts)
	}

	node.pendingCXReceipts = make(map[string]*types.CXReceiptsProof)
	for _, v := range pendingReceiptsList {
		blockNum := v.Header.Number().Uint64()
		shardID := v.Header.ShardID()
		key := utils.GetPendingCXKey(shardID, blockNum)
		node.pendingCXReceipts[key] = v
	}

	utils.Logger().Debug().Msgf("[proposeReceiptsProof] number of validReceipts %d", len(validReceiptsList))
	return validReceiptsList
}
