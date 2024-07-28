package consensus

import (
	"bytes"
	"context"
	"encoding/hex"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	bls2 "github.com/harmony-one/bls/ffi/go/bls"
	"github.com/harmony-one/harmony/consensus/signature"

	nodeconfig "github.com/harmony-one/harmony/internal/configs/node"
	"github.com/harmony-one/harmony/internal/utils"

	"github.com/rs/zerolog"

	msg_pb "github.com/harmony-one/harmony/api/proto/message"
	"github.com/harmony-one/harmony/block"
	"github.com/harmony-one/harmony/consensus/quorum"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/crypto/bls"
	vrf_bls "github.com/harmony-one/harmony/crypto/vrf/bls"
	"github.com/harmony-one/harmony/p2p"
	"github.com/harmony-one/harmony/shard"
	"github.com/harmony-one/vdf/src/vdf_go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	errSenderPubKeyNotLeader  = errors.New("sender pubkey doesn't match leader")
	errVerifyMessageSignature = errors.New("verify message signature failed")
	errParsingFBFTMessage     = errors.New("failed parsing FBFT message")
)

// timeout constant
const (
	// CommitSigSenderTimeout is the timeout for sending the commit sig to finish block proposal
	CommitSigSenderTimeout = 10 * time.Second
	// CommitSigReceiverTimeout is the timeout for the receiving side of the commit sig
	// if timeout, the receiver should instead ready directly from db for the commit sig
	CommitSigReceiverTimeout = 8 * time.Second
)

// IsViewChangingMode return true if curernt mode is viewchanging
func (consensus *Consensus) IsViewChangingMode() bool {
	return consensus.current.Mode() == ViewChanging
}

// HandleMessageUpdate will update the consensus state according to received message
func (consensus *Consensus) HandleMessageUpdate(ctx context.Context, msg *msg_pb.Message, senderKey *bls.SerializedPublicKey, divisionFlag *bool) error {
	// when node is in ViewChanging mode, it still accepts normal messages into FBFTLog
	// in order to avoid possible trap forever but drop PREPARE and COMMIT
	// which are message types specifically for a node acting as leader
	// so we just ignore those messages
	if consensus.IsViewChangingMode() &&
		(msg.Type == msg_pb.MessageType_PREPARE ||
			msg.Type == msg_pb.MessageType_COMMIT) {
		return nil
	}

	// Do easier check before signature check
	if msg.Type == msg_pb.MessageType_ANNOUNCE || msg.Type == msg_pb.MessageType_PREPARED || msg.Type == msg_pb.MessageType_COMMITTED {
		// Only validator needs to check whether the message is from the correct leader
		if !bytes.Equal(senderKey[:], consensus.LeaderPubKey.Bytes[:]) &&
			consensus.current.Mode() == Normal && !consensus.IgnoreViewIDCheck.IsSet() {
			return errSenderPubKeyNotLeader
		}
	}

	if msg.Type != msg_pb.MessageType_PREPARE && msg.Type != msg_pb.MessageType_COMMIT {
		// Leader doesn't need to check validator's message signature since the consensus signature will be checked
		if !consensus.senderKeySanityChecks(msg, senderKey) {
			return errVerifyMessageSignature
		}
	}

	// Parse FBFT message
	var fbftMsg *FBFTMessage
	var err error
	switch t := msg.Type; true {
	case t == msg_pb.MessageType_VIEWCHANGE:
		fbftMsg, err = ParseViewChangeMessage(msg)
	case t == msg_pb.MessageType_NEWVIEW:
		members := consensus.Decider.Participants()
		fbftMsg, err = ParseNewViewMessage(msg, members)
	default:
		fbftMsg, err = consensus.ParseFBFTMessage(msg)
	}
	if err != nil || fbftMsg == nil {
		return errors.Wrapf(err, "unable to parse consensus msg with type: %s", msg.Type)
	}

	canHandleViewChange := true
	intendedForValidator, intendedForLeader :=
		!consensus.IsLeader(),
		consensus.IsLeader()

	// if in backup normal mode, force ignore view change event and leader event.
	if consensus.current.Mode() == NormalBackup {
		canHandleViewChange = false
		intendedForLeader = false
	}

	// Route message to handler
	switch t := msg.Type; true {
	// Handle validator intended messages first
	case t == msg_pb.MessageType_ANNOUNCE && intendedForValidator:
		consensus.onAnnounce(msg)
	case t == msg_pb.MessageType_PREPARED && intendedForValidator:
		consensus.onPrepared(fbftMsg)
	case t == msg_pb.MessageType_COMMITTED && intendedForValidator:
		consensus.onCommitted(fbftMsg, divisionFlag)

	// Handle leader intended messages now
	case t == msg_pb.MessageType_PREPARE && intendedForLeader:
		consensus.onPrepare(fbftMsg)
	case t == msg_pb.MessageType_COMMIT && intendedForLeader:
		consensus.onCommit(fbftMsg, divisionFlag)

	// Handle view change messages
	case t == msg_pb.MessageType_VIEWCHANGE && canHandleViewChange:
		consensus.onViewChange(fbftMsg)
	case t == msg_pb.MessageType_NEWVIEW && canHandleViewChange:
		consensus.onNewView(fbftMsg)
	}

	return nil
}

func (consensus *Consensus) finalCommit(divisionFlag *bool) {
	numCommits := consensus.Decider.SignersCount(quorum.Commit)

	consensus.getLogger().Info().
		Int64("NumCommits", numCommits).
		Msg("[finalCommit] Finalizing Consensus")
	beforeCatchupNum := consensus.blockNum

	leaderPriKey, err := consensus.GetConsensusLeaderPrivateKey()
	if err != nil {
		consensus.getLogger().Error().Err(err).Msg("[finalCommit] leader not found")
		return
	}
	// Construct committed message
	network, err := consensus.construct(msg_pb.MessageType_COMMITTED, nil, []*bls.PrivateKeyWrapper{leaderPriKey})
	if err != nil {
		consensus.getLogger().Warn().Err(err).
			Msg("[finalCommit] Unable to construct Committed message")
		return
	}
	msgToSend, FBFTMsg :=
		network.Bytes,
		network.FBFTMsg
	commitSigAndBitmap := FBFTMsg.Payload
	consensus.FBFTLog.AddVerifiedMessage(FBFTMsg)
	// find correct block content
	curBlockHash := consensus.blockHash
	block := consensus.FBFTLog.GetBlockByHash(curBlockHash)
	if block == nil {
		consensus.getLogger().Warn().
			Str("blockHash", hex.EncodeToString(curBlockHash[:])).
			Msg("[finalCommit] Cannot find block by hash")
		return
	}

	if err := consensus.verifyLastCommitSig(commitSigAndBitmap, block); err != nil {
		consensus.getLogger().Warn().Err(err).Msg("[finalCommit] failed verifying last commit sig")
		return
	}
	consensus.getLogger().Info().Hex("new", commitSigAndBitmap).Msg("[finalCommit] Overriding commit signatures!!")
	consensus.Blockchain.WriteCommitSig(block.NumberU64(), commitSigAndBitmap)

	block.SetCurrentCommitSig(commitSigAndBitmap)
	err = consensus.commitBlock(block, FBFTMsg, divisionFlag)

	if err != nil || consensus.blockNum-beforeCatchupNum != 1 {
		consensus.getLogger().Err(err).
			Uint64("beforeCatchupBlockNum", beforeCatchupNum).
			Msg("[finalCommit] Leader failed to commit the confirmed block")
	}

	// if leader successfully finalizes the block, send committed message to validators
	// Note: leader already sent 67% commit in preCommit. The 100% commit won't be sent immediately
	// to save network traffic. It will only be sent in retry if consensus doesn't move forward.
	// Or if the leader is changed for next block, the 100% committed sig will be sent to the next leader immediately.
	if !consensus.IsLeader() || block.IsLastBlockInEpoch() {
		// send immediately
		if err := consensus.msgSender.SendWithRetry(
			block.NumberU64(),
			msg_pb.MessageType_COMMITTED, []nodeconfig.GroupID{
				nodeconfig.NewGroupIDByShardID(nodeconfig.ShardID(consensus.ShardID)),
			},
			p2p.ConstructMessage(msgToSend)); err != nil {
			consensus.getLogger().Warn().Err(err).Msg("[finalCommit] Cannot send committed message")
		} else {
			consensus.getLogger().Info().
				Hex("blockHash", curBlockHash[:]).
				Uint64("blockNum", consensus.blockNum).
				Msg("[finalCommit] Sent Committed Message")
		}
		consensus.getLogger().Info().Msg("[finalCommit] Start consensus timer")
		consensus.consensusTimeout[timeoutConsensus].Start()
	} else {
		// delayed send
		consensus.msgSender.DelayedSendWithRetry(
			block.NumberU64(),
			msg_pb.MessageType_COMMITTED, []nodeconfig.GroupID{
				nodeconfig.NewGroupIDByShardID(nodeconfig.ShardID(consensus.ShardID)),
			},
			p2p.ConstructMessage(msgToSend))
		consensus.getLogger().Info().
			Hex("blockHash", curBlockHash[:]).
			Uint64("blockNum", consensus.blockNum).
			Hex("lastCommitSig", commitSigAndBitmap).
			Msg("[finalCommit] Queued Committed Message")
	}

	// Dump new block into level db
	// In current code, we add signatures in block in tryCatchup, the block dump to explorer does not contains signatures
	// but since explorer doesn't need signatures, it should be fine
	// in future, we will move signatures to next block
	//explorer.GetStorageInstance(consensus.leader.IP, consensus.leader.Port, true).Dump(block, beforeCatchupNum)

	if consensus.consensusTimeout[timeoutBootstrap].IsActive() {
		consensus.consensusTimeout[timeoutBootstrap].Stop()
		consensus.getLogger().Info().Msg("[finalCommit] stop bootstrap timer only once")
	}

	// 记录一下现在的时间，用于记录每个区块的时间
	consensus.getLogger().Info().
		Uint64("blockNum", block.NumberU64()).
		Uint64("epochNum", block.Epoch().Uint64()).
		Uint64("ViewId", block.Header().ViewID().Uint64()).
		Str("blockHash", block.Hash().String()).
		Int("numTxns", len(block.Transactions())).
		Int("numStakingTxns", len(block.StakingTransactions())).
		Float64("consensus time", float64(time.Since(time.Unix(block.Header().Time().Int64(), 0)).Seconds())).
		Uint32("divisionLevel", block.Header().DivisionLevel()).
		Interface("BlockIDs", block.Header().BlockIDs()).
		Int("number TransactionReceiptMsg", len(block.Header().TransactionReceiptMsg())).
		// Interface("TransactionReceiptMsg").
		Msg("HOORAY!!!!!!! CONSENSUS REACHED!!!!!!!")

		// uhhBot test 不在这儿处理交易后半段了
	// //处理baseline intra-shard 交易
	// for _, item := range block.Header().TransactionMsg() {
	// 	if item.ToShard == item.FromShard {
	// 		consensus.getLogger().Info().
	// 			Uint32("count", item.Count).
	// 			// confirming transactions
	// 			Str("", "ct").
	// 			Float64("latency", float64(time.Since(time.Unix(block.Header().Time().Int64(), 0)).Seconds())).
	// 			Msg("")
	// 	}
	// }

	// // 这个区块的交易完成了，所以我要处理对应的交易
	// txnsMsg := append([]blockif.TransactionMsg(nil), block.Header().TransactionReceiptMsg()...)
	// for index, item := range txnsMsg {
	// 	if !item.IsMeta {
	// 		// baseline cross-shard 第二段
	// 		consensus.getLogger().Info().
	// 			Uint32("fromShard", item.FromShard).
	// 			Uint32("count", item.Count).
	// 			Str("", "ctx").
	// 			Float64("latency", float64(time.Since(time.Unix(int64(item.BeginTime), 0)).Seconds())).
	// 			Msg("")
	// 	} else {
	// 		// meta的交易
	// 		if item.FromShard == consensus.ShardID && item.ToShard == consensus.ShardID && item.ToMeta == item.FromMeta {
	// 			// 我自己meta的meta内交易
	// 			consensus.getLogger().Info().
	// 				Uint32("count", item.Count).
	// 				Uint32("ShardID", item.FromShard).
	// 				Uint32("Meta", item.ToMeta).
	// 				Str("", "ctm").
	// 				Float64("latency", float64(time.Since(time.Unix(int64(item.BeginTime), 0)).Seconds())).
	// 				Msg("")
	// 		} else if !item.FromOK {
	// 			// 发送给对应的meta让他们自己处理一下先
	// 			// 我猜是把header发过去啦
	// 			tmpName := nodeconfig.NewGroupIDByHorizontalShardID(nodeconfig.ShardID(item.ToShard),
	// 				consensus.DivisionLevel,
	// 				uint32(item.ToMeta),
	// 			)

	// 			txnsMsg[index].FromOK = true

	// 			metaData := &types.MetaGroupData_syncHeader{
	// 				BlockNum:    uint32(block.Number().Uint64()),
	// 				ShardID:     consensus.ShardID,
	// 				MetaGroupID: item.FromMeta,
	// 				IsCross:     true,
	// 				CrossTxns:   txnsMsg[index : index+1],
	// 			}
	// 			// uhhBottodo 这儿怎么只有full leader在发？实际上应该是这个小meta自个证明给对应的meta
	// 			go func() {
	// 				if err := consensus.msgSender.host.SendMessageToGroups([]nodeconfig.GroupID{tmpName},
	// 					p2p.ConstructMessage(proto_node.ConstructMetaHeaderData(metaData)),
	// 				); err != nil {
	// 					utils.Logger().Error().Err(err).
	// 						Str("groupName", tmpName.String()).
	// 						Msg("[sendCross_meta] Cannot Cross")
	// 				} else {
	// 					utils.Logger().Info().
	// 						Str("groupName", tmpName.String()).
	// 						Interface("metaData", metaData).
	// 						Msg("[sendCross_meta] send Cross!! testlin")
	// 				}
	// 			}()
	// 		} else {
	// 			// 跨meta交易的第二步
	// 			consensus.getLogger().Info().
	// 				Uint32("count", item.Count).
	// 				Uint32("FromShardID", item.FromShard).
	// 				Uint32("FromMeta", item.FromMeta).
	// 				Uint32("ToShardID", item.ToShard).
	// 				Uint32("ToMeta", item.ToMeta).
	// 				Str("", "ctmx").
	// 				Float64("latency", float64(time.Since(time.Unix(int64(item.BeginTime), 0)).Seconds())).
	// 				Msg("")
	// 		}
	// 	}
	// }

	waitTime := time.Duration(consensus.WaitTime) * time.Second

	// uhhBottodo
	if block.Header().DivisionLevel() > 1 {
		time.Sleep(time.Duration(consensus.StopMainFlag) * time.Second)
	} else {
		time.Sleep(waitTime)
	}
	consensus.LastConsensusReachedTime = time.Now()

	consensus.UpdateLeaderMetrics(float64(numCommits), float64(block.NumberU64()))

	// If still the leader, send commit sig/bitmap to finish the new block proposal,
	// else, the block proposal will timeout by itself.
	if consensus.IsLeader() {
		if block.IsLastBlockInEpoch() {
			// No pipelining
			go func() {
				consensus.getLogger().Info().Msg("[finalCommit] sending block proposal signal")
				consensus.ReadySignal <- SyncProposal
			}()
		} else {
			// pipelining
			go func() {
				select {
				case consensus.CommitSigChannel <- commitSigAndBitmap:
				case <-time.After(CommitSigSenderTimeout):
					utils.Logger().Error().Err(err).Msg("[finalCommit] channel not received after 6s for commitSigAndBitmap")
				}
			}()
		}
	}
}

// BlockCommitSigs returns the byte array of aggregated
// commit signature and bitmap signed on the block
func (consensus *Consensus) BlockCommitSigs(blockNum uint64) ([]byte, error) {
	if consensus.blockNum <= 1 {
		return nil, nil
	}
	lastCommits, err := consensus.Blockchain.ReadCommitSig(blockNum)
	if err != nil ||
		len(lastCommits) < bls.BLSSignatureSizeInBytes {
		msgs := consensus.FBFTLog.GetMessagesByTypeSeq(
			msg_pb.MessageType_COMMITTED, blockNum,
		)
		if len(msgs) != 1 {
			consensus.getLogger().Error().
				Int("numCommittedMsg", len(msgs)).
				Msg("GetLastCommitSig failed with wrong number of committed message")
			return nil, errors.Errorf(
				"GetLastCommitSig failed with wrong number of committed message %d", len(msgs),
			)
		}
		lastCommits = msgs[0].Payload
	}

	return lastCommits, nil
}

// Start waits for the next new block and run consensus
func (consensus *Consensus) Start(
	blockChannel chan *types.Block, stopChan, stoppedChan, startChannel chan struct{},
) {
	go func() {
		toStart := make(chan struct{}, 1)
		isInitialLeader := consensus.IsLeader()
		if isInitialLeader {
			consensus.getLogger().Info().Time("time", time.Now()).Msg("[ConsensusMainLoop] Waiting for consensus start")
			// send a signal to indicate it's ready to run consensus
			// this signal is consumed by node object to create a new block and in turn trigger a new consensus on it
			go func() {
				<-startChannel
				toStart <- struct{}{}
				consensus.getLogger().Info().Time("time", time.Now()).Msg("[ConsensusMainLoop] Send ReadySignal")
				consensus.ReadySignal <- SyncProposal
			}()
		}
		consensus.getLogger().Info().Time("time", time.Now()).Msg("[ConsensusMainLoop] Consensus started")
		defer close(stoppedChan)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		consensus.consensusTimeout[timeoutBootstrap].Start()
		consensus.getLogger().Info().Msg("[ConsensusMainLoop] Start bootstrap timeout (only once)")

		// Set up next block due time.
		// consensus.NextBlockDue = time.Now().Add(consensus.BlockPeriod * time.Second)
		// 时间
		// consensus.NextBlockDue = time.Now().Add(time.Duration(consensus.WaitTime) * time.Second)
		consensus.NextBlockDue = time.Now()
		start := false
		for {
			select {
			case <-toStart:
				start = true
			case <-ticker.C:
				if !start && isInitialLeader {
					continue
				}
				for k, v := range consensus.consensusTimeout {
					// stop timer in listening mode
					if consensus.current.Mode() == Listening {
						v.Stop()
						continue
					}

					if consensus.current.Mode() == Syncing {
						// never stop bootstrap timer here in syncing mode as it only starts once
						// if it is stopped, bootstrap will be stopped and nodes
						// can't start view change or join consensus
						// the bootstrap timer will be stopped once consensus is reached or view change
						// is succeeded
						if k != timeoutBootstrap {
							consensus.getLogger().Debug().
								Str("k", k.String()).
								Str("Mode", consensus.current.Mode().String()).
								Msg("[ConsensusMainLoop] consensusTimeout stopped!!!")
							v.Stop()
							continue
						}
					}
					if !v.CheckExpire() {
						continue
					}
					if k != timeoutViewChange {
						// Cut message
						// consensus.getLogger().Warn().Msg("[ConsensusMainLoop] Ops Consensus Timeout!!!")
						// consensus.startViewChange()
						// 不进入viewchange
						break
					} else {
						consensus.getLogger().Warn().Msg("[ConsensusMainLoop] Ops View Change Timeout!!!")
						consensus.startViewChange()
						break
					}
				}

			// TODO: Refactor this piece of code to consensus/downloader.go after DNS legacy sync is removed
			case <-consensus.syncReadyChan:
				consensus.getLogger().Info().Msg("[ConsensusMainLoop] syncReadyChan")
				consensus.mutex.Lock()
				if consensus.blockNum < consensus.Blockchain.CurrentHeader().Number().Uint64()+1 {
					consensus.SetBlockNum(consensus.Blockchain.CurrentHeader().Number().Uint64() + 1)
					consensus.SetViewIDs(consensus.Blockchain.CurrentHeader().ViewID().Uint64() + 1)
					mode := consensus.UpdateConsensusInformation()
					consensus.current.SetMode(mode)
					consensus.getLogger().Info().Msg("[syncReadyChan] Start consensus timer")
					consensus.consensusTimeout[timeoutConsensus].Start()
					consensus.getLogger().Info().Str("Mode", mode.String()).Msg("Node is IN SYNC")
					consensusSyncCounterVec.With(prometheus.Labels{"consensus": "in_sync"}).Inc()
				} else if consensus.Mode() == Syncing {
					// Corner case where sync is triggered before `onCommitted` and there is a race
					// for block insertion between consensus and downloader.
					mode := consensus.UpdateConsensusInformation()
					consensus.SetMode(mode)
					consensus.getLogger().Info().Msg("[syncReadyChan] Start consensus timer")
					consensus.consensusTimeout[timeoutConsensus].Start()
					consensusSyncCounterVec.With(prometheus.Labels{"consensus": "in_sync"}).Inc()
				}
				consensus.mutex.Unlock()

			// TODO: Refactor this piece of code to consensus/downloader.go after DNS legacy sync is removed
			case <-consensus.syncNotReadyChan:
				consensus.getLogger().Info().Msg("[ConsensusMainLoop] syncNotReadyChan")
				consensus.SetBlockNum(consensus.Blockchain.CurrentHeader().Number().Uint64() + 1)
				consensus.current.SetMode(Syncing)
				consensus.getLogger().Info().Msg("[ConsensusMainLoop] Node is OUT OF SYNC")
				consensusSyncCounterVec.With(prometheus.Labels{"consensus": "out_of_sync"}).Inc()

			case newBlock := <-blockChannel:
				consensus.getLogger().Info().
					Uint64("MsgBlockNum", newBlock.NumberU64()).
					Msg("[ConsensusMainLoop] Received Proposed New Block!")

				if newBlock.NumberU64() < consensus.blockNum {
					consensus.getLogger().Warn().Uint64("newBlockNum", newBlock.NumberU64()).
						Msg("[ConsensusMainLoop] received old block, abort")
					continue
				}
				// Sleep to wait for the full block time
				consensus.getLogger().Info().Msg("[ConsensusMainLoop] Waiting for Block Time")
				// uhhBot here NextBLockDue is changed
				if consensus.FirstTransactionTime <= consensus.current.blockViewID {
					// <-time.After(time.Until(time.Now().Add(time.Duration(consensus.WaitTime) * time.Second)))
					<-time.After(time.Until(consensus.NextBlockDue))

					consensus.getLogger().Info().Int("waiting time", consensus.WaitTime).Msg("[ConsensusMainLoop] waiting finished")
					// consensus.BlockPeriod = time.Duration(consensus.WaitTime)
				}
				consensus.StartFinalityCount()

				// Update time due for next block
				// 时间
				// consensus.NextBlockDue = time.Now().Add(time.Duration(consensus.WaitTime) * time.Second)

				consensus.NextBlockDue = time.Now()

				startTime = time.Now()
				consensus.msgSender.Reset(newBlock.NumberU64())

				consensus.getLogger().Info().
					Int("numTxs", len(newBlock.Transactions())).
					Int("numStakingTxs", len(newBlock.StakingTransactions())).
					Time("startTime", startTime).
					Int64("publicKeys", consensus.Decider.ParticipantsCount()).
					Msg("[ConsensusMainLoop] STARTING CONSENSUS")
				consensus.announce(newBlock)
			case <-stopChan:
				consensus.getLogger().Info().Msg("[ConsensusMainLoop] stopChan")
				return
			}
		}
		consensus.getLogger().Info().Msg("[ConsensusMainLoop] Ended.")
	}()

	if consensus.dHelper != nil {
		consensus.dHelper.start()
	}
}

// Close close the consensus. If current is in normal commit phase, wait until the commit
// phase end.
func (consensus *Consensus) Close() error {
	if consensus.dHelper != nil {
		consensus.dHelper.close()
	}
	consensus.waitForCommit()
	return nil
}

// ???
// waitForCommit wait extra 2 seconds for commit phase to finish
func (consensus *Consensus) waitForCommit() {
	if consensus.Mode() != Normal || consensus.phase != FBFTCommit {
		return
	}
	// We only need to wait consensus is in normal commit phase
	utils.Logger().Warn().Str("phase", consensus.phase.String()).Msg("[shutdown] commit phase has to wait")

	maxWait := time.Now().Add(2 * consensus.BlockPeriod)
	for time.Now().Before(maxWait) && consensus.GetConsensusPhase() == "Commit" {
		utils.Logger().Warn().Msg("[shutdown] wait for consensus finished")
		time.Sleep(time.Millisecond * 100)
	}
}

// LastMileBlockIter is the iterator to iterate over the last mile blocks in consensus cache.
// All blocks returned are guaranteed to pass the verification.
type LastMileBlockIter struct {
	blockCandidates []*types.Block
	fbftLog         *FBFTLog
	verify          func(*types.Block) error
	curIndex        int
	logger          *zerolog.Logger
}

// GetLastMileBlockIter get the iterator of the last mile blocks starting from number bnStart
func (consensus *Consensus) GetLastMileBlockIter(bnStart uint64) (*LastMileBlockIter, error) {
	consensus.mutex.Lock()
	defer consensus.mutex.Unlock()

	if consensus.BlockVerifier == nil {
		return nil, errors.New("consensus haven't initialized yet")
	}
	blocks, _, err := consensus.getLastMileBlocksAndMsg(bnStart)
	if err != nil {
		return nil, err
	}
	return &LastMileBlockIter{
		blockCandidates: blocks,
		fbftLog:         consensus.FBFTLog,
		verify:          consensus.BlockVerifier,
		curIndex:        0,
		logger:          consensus.getLogger(),
	}, nil
}

// Next iterate to the next last mile block
func (iter *LastMileBlockIter) Next() *types.Block {
	if iter.curIndex >= len(iter.blockCandidates) {
		return nil
	}
	block := iter.blockCandidates[iter.curIndex]
	iter.curIndex++

	if !iter.fbftLog.IsBlockVerified(block) {
		if err := iter.verify(block); err != nil {
			iter.logger.Debug().Err(err).Msg("block verification failed in consensus last mile block")
			return nil
		}
		iter.fbftLog.MarkBlockVerified(block)
	}
	return block
}

func (consensus *Consensus) getLastMileBlocksAndMsg(bnStart uint64) ([]*types.Block, []*FBFTMessage, error) {
	var (
		blocks []*types.Block
		msgs   []*FBFTMessage
	)
	for blockNum := bnStart; ; blockNum++ {
		blk, msg, err := consensus.FBFTLog.GetCommittedBlockAndMsgsFromNumber(blockNum, consensus.getLogger())
		if err != nil {
			if err == errFBFTLogNotFound {
				break
			}
			return nil, nil, err
		}
		blocks = append(blocks, blk)
		msgs = append(msgs, msg)
	}
	return blocks, msgs, nil
}

// preCommitAndPropose commit the current block with 67% commit signatures and start
// proposing new block which will wait on the full commit signatures to finish
func (consensus *Consensus) preCommitAndPropose(blk *types.Block) error {
	if blk == nil {
		return errors.New("block to pre-commit is nil")
	}

	leaderPriKey, err := consensus.GetConsensusLeaderPrivateKey()
	if err != nil {
		consensus.getLogger().Error().Err(err).Msg("[preCommitAndPropose] leader not found")
		return err
	}

	// Construct committed message
	network, err := consensus.construct(msg_pb.MessageType_COMMITTED, nil, []*bls.PrivateKeyWrapper{leaderPriKey})
	if err != nil {
		consensus.getLogger().Warn().Err(err).
			Msg("[preCommitAndPropose] Unable to construct Committed message")
		return err
	}

	msgToSend, FBFTMsg :=
		network.Bytes,
		network.FBFTMsg
	bareMinimumCommit := FBFTMsg.Payload
	consensus.FBFTLog.AddVerifiedMessage(FBFTMsg)

	if err := consensus.verifyLastCommitSig(bareMinimumCommit, blk); err != nil {
		return errors.Wrap(err, "[preCommitAndPropose] failed verifying last commit sig")
	}

	go func() {
		blk.SetCurrentCommitSig(bareMinimumCommit)

		if _, err := consensus.Blockchain.InsertChain([]*types.Block{blk}, !consensus.FBFTLog.IsBlockVerified(blk)); err != nil {
			consensus.getLogger().Error().Err(err).Msg("[preCommitAndPropose] Failed to add block to chain")
			return
		}

		// if leader successfully finalizes the block, send committed message to validators
		if err := consensus.msgSender.SendWithRetry(
			blk.NumberU64(),
			msg_pb.MessageType_COMMITTED, []nodeconfig.GroupID{
				nodeconfig.NewGroupIDByShardID(nodeconfig.ShardID(consensus.ShardID)),
			},
			p2p.ConstructMessage(msgToSend)); err != nil {
			consensus.getLogger().Warn().Err(err).Msg("[preCommitAndPropose] Cannot send committed message")
		} else {
			consensus.getLogger().Info().
				Str("blockHash", blk.Hash().Hex()).
				Uint64("blockNum", consensus.blockNum).
				Hex("lastCommitSig", bareMinimumCommit).
				Int("size of msg", len(msgToSend)).
				Msg("[preCommitAndPropose] Sent Committed Message")
		}
		consensus.getLogger().Info().Msg("[preCommitAndPropose] Start consensus timer")
		consensus.consensusTimeout[timeoutConsensus].Start()

		// Send signal to Node to propose the new block for consensus
		consensus.getLogger().Info().Msg("[preCommitAndPropose] sending block proposal signal")

		consensus.ReadySignal <- AsyncProposal
	}()

	return nil
}

func (consensus *Consensus) verifyLastCommitSig(lastCommitSig []byte, blk *types.Block) error {
	if len(lastCommitSig) < bls.BLSSignatureSizeInBytes {
		return errors.New("lastCommitSig not have enough length")
	}

	aggSigBytes := lastCommitSig[0:bls.BLSSignatureSizeInBytes]

	aggSig := bls2.Sign{}
	err := aggSig.Deserialize(aggSigBytes)

	if err != nil {
		return errors.New("unable to deserialize multi-signature from payload")
	}
	aggPubKey := consensus.commitBitmap.AggregatePublic

	commitPayload := signature.ConstructCommitPayload(consensus.Blockchain,
		blk.Epoch(), blk.Hash(), blk.NumberU64(), blk.Header().ViewID().Uint64())

	if !aggSig.VerifyHash(aggPubKey, commitPayload) {
		return errors.New("Failed to verify the multi signature for last commit sig")
	}
	return nil
}

// tryCatchup add the last mile block in PBFT log memory cache to blockchain.
func (consensus *Consensus) tryCatchup(divisionFlag *bool) error {
	// TODO: change this to a more systematic symbol
	if consensus.BlockVerifier == nil {
		return errors.New("consensus haven't finished initialization")
	}
	initBN := consensus.blockNum
	defer consensus.postCatchup(initBN)

	blks, msgs, err := consensus.getLastMileBlocksAndMsg(initBN)
	if err != nil {
		return errors.Wrapf(err, "[TryCatchup] Failed to get last mile blocks: %v", err)
	}
	for i := range blks {
		blk, msg := blks[i], msgs[i]
		if blk == nil {
			return nil
		}
		blk.SetCurrentCommitSig(msg.Payload)

		if err := consensus.VerifyBlock(blk); err != nil {
			consensus.getLogger().Err(err).Msg("[TryCatchup] failed block verifier")
			return err
		}
		consensus.getLogger().Info().Msg("[TryCatchup] Adding block to chain")
		if err := consensus.commitBlock(blk, msgs[i], divisionFlag); err != nil {
			consensus.getLogger().Error().Err(err).Msg("[TryCatchup] Failed to add block to chain")
			return err
		}
		select {
		// TODO: Remove this when removing dns sync and stream sync is fully up
		case consensus.VerifiedNewBlock <- blk:
		default:
			consensus.getLogger().Info().
				Str("blockHash", blk.Hash().String()).
				Msg("[TryCatchup] consensus verified block send to chan failed")
			continue
		}
	}
	return nil
}

func (consensus *Consensus) commitBlock(blk *types.Block, committedMsg *FBFTMessage, divisionFlag *bool) error {
	if consensus.Blockchain.CurrentBlock().NumberU64() < blk.NumberU64() {
		if _, err := consensus.Blockchain.InsertChain([]*types.Block{blk}, !consensus.FBFTLog.IsBlockVerified(blk)); err != nil {
			consensus.getLogger().Error().Err(err).Msg("[commitBlock] Failed to add block to chain")
			return err
		}
	}

	if !committedMsg.HasSingleSender() {
		consensus.getLogger().Error().Msg("[TryCatchup] Leader message can not have multiple sender keys")
		return errIncorrectSender
	}
	consensus.FinishFinalityCount()
	consensus.PostConsensusJob(blk)
	consensus.SetupForNewConsensus(blk, committedMsg)
	utils.Logger().Info().Uint64("blockNum", blk.NumberU64()).
		Str("hash", blk.Header().Hash().Hex()).
		Msg("Added New Block to Blockchain!!!")

	// uhhBot now we try to add one more shard

	// if blk.NumberU64()%4 == 0 && (consensus.MetaGroupSize == 0 || (consensus.DivisionLevel*consensus.MetaGroupSize)/(consensus.DivisionLevel+1) >= 4) {
	// 	// if blk.NumberU64()%4 == 0 {
	// 	utils.Logger().Info().Uint64("blockNum", blk.NumberU64()).
	// 		Msg("uhhBot, try to divide blockchain")
	// 	// 需要获得节点
	// 	newLevel := consensus.DivisionLevel + 1
	// 	consensus.DivisionLevel = newLevel
	// 	*divisionFlag = true
	// }

	// 在这儿进行属性修改
	// uhhBoteme metaShard 一步拉满

	// 临时 小共识开始阶段
	if blk.NumberU64() == 3 {
		// if blk.NumberU64()%4 == 0 {
		// 需要获得节点
		var tmpLevel uint32 = 0
		switch shard.Schedule.InstanceForEpoch(common.Big0).NumNodesPerShard() {
		case 420:
			tmpLevel = 5
		case 500:
			tmpLevel = 6
		case 600:
			tmpLevel = 7
		case 40:
			tmpLevel = 5
		case 90:
			tmpLevel = 5
		case 100:
			tmpLevel = 2
		case 180:
			tmpLevel = 3
		case 200:
			tmpLevel = 3
		case 210:
			tmpLevel = 3
		case 20:
			tmpLevel = 2
		case 30:
			tmpLevel = 3
		case 320:
			tmpLevel = 4
		case 430:
			tmpLevel = 5
		case 480:
			tmpLevel = 5
		case 510:
			tmpLevel = 5
		// gearBox
		// uhhBot test gearbox malicious
		case 640:
			tmpLevel = 5
		case 1290:
			tmpLevel = 8
		case 1920:
			tmpLevel = 11
		case 2550:
			tmpLevel = 11
		default:
			tmpLevel = 2
		}

		consensus.DivisionLevel = tmpLevel
		// 临时 兼容gearbox
		if shard.Schedule.InstanceForEpoch(common.Big0).NumNodesPerShard() < 600 && shard.Schedule.InstanceForEpoch(common.Big0).NumNodesPerShard() != 30 {
			ins := shard.Schedule.InstanceForEpoch(common.Big0)
			numShards := ins.NumShards()
			shardSize := ins.NumNodesPerShard()
			var indexInFull uint32 = consensus.Index / numShards
			shardID := consensus.ShardID
			indexInMeta := indexInFull / consensus.DivisionLevel

			consensus.MetaGroupID = indexInFull % consensus.DivisionLevel
			consensus.MetaGroupSize = uint32(shardSize) / consensus.DivisionLevel
			consensus.MetaGroupName =
				nodeconfig.NewGroupIDByHorizontalShardID(nodeconfig.ShardID(shardID),
					consensus.DivisionLevel,
					uint32(consensus.MetaGroupID),
				).String()
			consensus.IsMetaLeader = false
			// 这里可能要修改
			if indexInMeta == 1 {
				consensus.IsMetaLeader = true
			}
			*divisionFlag = true

		} else {
			ins := shard.Schedule.InstanceForEpoch(common.Big0)
			numShards := ins.NumShards()
			var indexInFull uint32 = consensus.Index / numShards
			switch shard.Schedule.InstanceForEpoch(common.Big0).NumNodesPerShard() {
			case 30:
				consensus.MetaGroupSize = 6
			case 640:
				// uhhBot test gearbox 临时
				consensus.MetaGroupSize = 41
			case 1290:
				consensus.MetaGroupSize = 75
			case 1920:
				consensus.MetaGroupSize = 76
			case 2550:
				consensus.MetaGroupSize = 80
			default:
				tmpLevel = 5
			}

			if indexInFull >= tmpLevel*consensus.MetaGroupSize {
				return nil
			}
			shardID := consensus.ShardID
			indexInMeta := indexInFull / consensus.DivisionLevel

			consensus.MetaGroupID = indexInFull % consensus.DivisionLevel
			consensus.MetaGroupName =
				nodeconfig.NewGroupIDByHorizontalShardID(nodeconfig.ShardID(shardID),
					consensus.DivisionLevel,
					uint32(consensus.MetaGroupID),
				).String()
			consensus.IsMetaLeader = false
			// 这里可能要修改
			if indexInMeta == 1 {
				consensus.IsMetaLeader = true
			}
			*divisionFlag = true
		}

		utils.Logger().Info().
			Uint32("Index", consensus.Index).
			Uint32("DivisionLevel", consensus.DivisionLevel).
			Uint32("MetaGroupID", consensus.MetaGroupID).
			Uint32("MetaGroupSize", consensus.MetaGroupSize).
			// Str("MetaGroupName", consensus.MetaGroupName).
			Bool("leader", consensus.IsMetaLeader).
			Msg("uhhBot, gearbox begin")
	}
	return nil
}

// SetupForNewConsensus sets the state for new consensus
func (consensus *Consensus) SetupForNewConsensus(blk *types.Block, committedMsg *FBFTMessage) {
	atomic.StoreUint64(&consensus.blockNum, blk.NumberU64()+1)
	consensus.SetCurBlockViewID(committedMsg.ViewID + 1)
	consensus.LeaderPubKey = committedMsg.SenderPubkeys[0]
	// Update consensus keys at last so the change of leader status doesn't mess up normal flow
	if blk.IsLastBlockInEpoch() {
		consensus.SetMode(consensus.UpdateConsensusInformation())
	}
	consensus.FBFTLog.PruneCacheBeforeBlock(blk.NumberU64())
	consensus.ResetState()
}

func (consensus *Consensus) postCatchup(initBN uint64) {
	if initBN < consensus.blockNum {
		consensus.getLogger().Info().
			Uint64("From", initBN).
			Uint64("To", consensus.blockNum).
			Msg("[TryCatchup] Caught up!")
		consensus.switchPhase("TryCatchup", FBFTAnnounce)
	}
	// catch up and skip from view change trap
	if initBN < consensus.blockNum && consensus.IsViewChangingMode() {
		consensus.current.SetMode(Normal)
		consensus.consensusTimeout[timeoutViewChange].Stop()
	}
}

// GenerateVrfAndProof generates new VRF/Proof from hash of previous block
func (consensus *Consensus) GenerateVrfAndProof(newHeader *block.Header) error {
	key, err := consensus.GetConsensusLeaderPrivateKey()
	if err != nil {
		return errors.New("[GenerateVrfAndProof] no leader private key provided")
	}
	sk := vrf_bls.NewVRFSigner(key.Pri)
	previousHeader := consensus.Blockchain.GetHeaderByNumber(
		newHeader.Number().Uint64() - 1,
	)
	if previousHeader == nil {
		return errors.New("[GenerateVrfAndProof] no parent header found")
	}

	previousHash := previousHeader.Hash()
	vrf, proof := sk.Evaluate(previousHash[:])
	if proof == nil {
		return errors.New("[GenerateVrfAndProof] failed to generate vrf")
	}

	newHeader.SetVrf(append(vrf[:], proof...))

	consensus.getLogger().Info().
		Uint64("BlockNum", newHeader.Number().Uint64()).
		Uint64("Epoch", newHeader.Epoch().Uint64()).
		Hex("VRF+Proof", newHeader.Vrf()).
		Msg("[GenerateVrfAndProof] Leader generated a VRF")

	return nil
}

// GenerateVdfAndProof generates new VDF/Proof from VRFs in the current epoch
func (consensus *Consensus) GenerateVdfAndProof(newBlock *types.Block, vrfBlockNumbers []uint64) {
	//derive VDF seed from VRFs generated in the current epoch
	seed := [32]byte{}
	for i := 0; i < consensus.VdfSeedSize(); i++ {
		previousVrf := consensus.Blockchain.GetVrfByNumber(vrfBlockNumbers[i])
		for j := 0; j < len(seed); j++ {
			seed[j] = seed[j] ^ previousVrf[j]
		}
	}

	consensus.getLogger().Info().
		Uint64("MsgBlockNum", newBlock.NumberU64()).
		Uint64("Epoch", newBlock.Header().Epoch().Uint64()).
		Int("Num of VRF", len(vrfBlockNumbers)).
		Msg("[ConsensusMainLoop] VDF computation started")

	// TODO ek – limit concurrency
	go func() {
		vdf := vdf_go.New(shard.Schedule.VdfDifficulty(), seed)
		outputChannel := vdf.GetOutputChannel()
		start := time.Now()
		vdf.Execute()
		duration := time.Since(start)
		consensus.getLogger().Info().
			Dur("duration", duration).
			Msg("[ConsensusMainLoop] VDF computation finished")
		output := <-outputChannel

		// The first 516 bytes are the VDF+proof and the last 32 bytes are XORed VRF as seed
		rndBytes := [548]byte{}
		copy(rndBytes[:516], output[:])
		copy(rndBytes[516:], seed[:])
		consensus.RndChannel <- rndBytes
	}()
}

// ValidateVdfAndProof validates the VDF/proof in the current epoch
func (consensus *Consensus) ValidateVdfAndProof(headerObj *block.Header) bool {
	vrfBlockNumbers, err := consensus.Blockchain.ReadEpochVrfBlockNums(headerObj.Epoch())
	if err != nil {
		consensus.getLogger().Error().Err(err).
			Str("MsgBlockNum", headerObj.Number().String()).
			Msg("[OnAnnounce] failed to read VRF block numbers for VDF computation")
	}

	//extra check to make sure there's no index out of range error
	//it can happen if epoch is messed up, i.e. VDF ouput is generated in the next epoch
	if consensus.VdfSeedSize() > len(vrfBlockNumbers) {
		return false
	}

	seed := [32]byte{}
	for i := 0; i < consensus.VdfSeedSize(); i++ {
		previousVrf := consensus.Blockchain.GetVrfByNumber(vrfBlockNumbers[i])
		for j := 0; j < len(seed); j++ {
			seed[j] = seed[j] ^ previousVrf[j]
		}
	}

	vdfObject := vdf_go.New(shard.Schedule.VdfDifficulty(), seed)
	vdfOutput := [516]byte{}
	copy(vdfOutput[:], headerObj.Vdf())
	if vdfObject.Verify(vdfOutput) {
		consensus.getLogger().Info().
			Str("MsgBlockNum", headerObj.Number().String()).
			Int("Num of VRF", consensus.VdfSeedSize()).
			Msg("[OnAnnounce] validated a new VDF")

	} else {
		consensus.getLogger().Warn().
			Str("MsgBlockNum", headerObj.Number().String()).
			Uint64("Epoch", headerObj.Epoch().Uint64()).
			Int("Num of VRF", consensus.VdfSeedSize()).
			Msg("[OnAnnounce] VDF proof is not valid")
		return false
	}

	return true
}
