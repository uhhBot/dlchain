package node

import (
	"bytes"
	"context"
	"math/rand"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/rlp"
	"github.com/harmony-one/harmony/api/proto"
	msg_pb "github.com/harmony-one/harmony/api/proto/message"
	proto_node "github.com/harmony-one/harmony/api/proto/node"
	"github.com/harmony-one/harmony/block"
	blockif "github.com/harmony-one/harmony/block/interface"
	"github.com/harmony-one/harmony/consensus"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/crypto/bls"
	nodeconfig "github.com/harmony-one/harmony/internal/configs/node"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/p2p"
	"github.com/harmony-one/harmony/shard"
	"github.com/harmony-one/harmony/staking/availability"
	"github.com/harmony-one/harmony/staking/slash"
	staking "github.com/harmony-one/harmony/staking/types"
	"github.com/harmony-one/harmony/webhooks"
	libp2p_peer "github.com/libp2p/go-libp2p-core/peer"
	libp2p_pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/semaphore"
)

const p2pMsgPrefixSize = 5
const p2pNodeMsgPrefixSize = proto.MessageTypeBytes + proto.MessageCategoryBytes

// some messages have uninteresting fields in header, slash, receipt and crosslink are
// such messages. This function assumes that input bytes are a slice which already
// past those not relevant header bytes.
func (node *Node) processSkippedMsgTypeByteValue(
	cat proto_node.BlockMessageType, content []byte,
) {
	switch cat {
	case proto_node.SlashCandidate:
		node.processSlashCandidateMessage(content)
	case proto_node.Receipt:
		node.ProcessReceiptMessage(content)
	case proto_node.CrossLink:
		node.ProcessCrossLinkMessage(content)
		// 我改了，增加对CXContract的处理
	case proto_node.ReceiptDIY:
		node.ProcessReceiptMessageDIY(content)
	case proto_node.HoriConsensus:
		node.ProcessMetaConsensusMsg(content)
	case proto_node.HoriBlockHeader:
		node.ProcessMetaHeaderMsg(content)
	default:
		utils.Logger().Error().
			Int("message-iota-value", int(cat)).
			Msg("Invariant usage of processSkippedMsgTypeByteValue violated")
	}
}

var (
	errInvalidPayloadSize         = errors.New("invalid payload size")
	errWrongBlockMsgSize          = errors.New("invalid block message size")
	latestSentCrosslinkNum uint64 = 0
)

// HandleNodeMessage parses the message and dispatch the actions.
func (node *Node) HandleNodeMessage(
	ctx context.Context,
	msgPayload []byte,
	actionType proto_node.MessageType,
) error {
	switch actionType {
	case proto_node.Transaction:
		node.transactionMessageHandler(msgPayload)
	case proto_node.Staking:
		node.stakingMessageHandler(msgPayload)
	case proto_node.Block:
		switch blockMsgType := proto_node.BlockMessageType(msgPayload[0]); blockMsgType {
		case proto_node.Sync:
			blocks := []*types.Block{}
			if err := rlp.DecodeBytes(msgPayload[1:], &blocks); err != nil {
				utils.Logger().Error().
					Err(err).
					Msg("block sync")
			} else {
				// for non-beaconchain node, subscribe to beacon block broadcast
				if node.Blockchain().ShardID() != shard.BeaconChainShardID {
					for _, block := range blocks {
						if block.ShardID() == 0 {
							utils.Logger().Info().
								Msgf("Beacon block being handled by block channel: %d", block.NumberU64())
							go func(blk *types.Block) {
								node.BeaconBlockChannel <- blk
							}(block)
						}
					}
				}
			}
		case
			proto_node.SlashCandidate,
			proto_node.Receipt,
			proto_node.CrossLink,
			// skip first byte which is blockMsgType
			// 我改了，增加对cross shard的处理
			proto_node.ReceiptDIY,
			proto_node.HoriConsensus,
			proto_node.HoriBlockHeader:
			node.processSkippedMsgTypeByteValue(blockMsgType, msgPayload[1:])
		}
	default:
		utils.Logger().Error().
			Str("Unknown actionType", string(actionType)).Msg("")
	}
	return nil
}

func (node *Node) transactionMessageHandler(msgPayload []byte) {
	txMessageType := proto_node.TransactionMessageType(msgPayload[0])

	switch txMessageType {
	case proto_node.Send:
		txs := types.Transactions{}
		err := rlp.Decode(bytes.NewReader(msgPayload[1:]), &txs) // skip the Send messge type
		if err != nil {
			utils.Logger().Error().
				Err(err).
				Msg("Failed to deserialize transaction list")
			return
		}
		node.addPendingTransactions(txs)
	}
}

func (node *Node) stakingMessageHandler(msgPayload []byte) {
	txMessageType := proto_node.TransactionMessageType(msgPayload[0])

	switch txMessageType {
	case proto_node.Send:
		txs := staking.StakingTransactions{}
		err := rlp.Decode(bytes.NewReader(msgPayload[1:]), &txs) // skip the Send messge type
		if err != nil {
			utils.Logger().Error().
				Err(err).
				Msg("Failed to deserialize staking transaction list")
			return
		}
		node.addPendingStakingTransactions(txs)
	}
}

// BroadcastNewBlock is called by consensus leader to sync new blocks with other clients/nodes.
// NOTE: For now, just send to the client (basically not broadcasting)
// TODO (lc): broadcast the new blocks to new nodes doing state sync
func (node *Node) BroadcastNewBlock(newBlock *types.Block) {
	groups := []nodeconfig.GroupID{node.NodeConfig.GetClientGroupID()}
	utils.Logger().Info().
		Msgf(
			"broadcasting new block %d, group %s", newBlock.NumberU64(), groups[0],
		)
	msg := p2p.ConstructMessage(
		proto_node.ConstructBlocksSyncMessage([]*types.Block{newBlock}),
	)
	if err := node.host.SendMessageToGroups(groups, msg); err != nil {
		utils.Logger().Warn().Err(err).Msg("cannot broadcast new block")
	}
}

// BroadcastSlash ..
func (node *Node) BroadcastSlash(witness *slash.Record) {
	if err := node.host.SendMessageToGroups(
		[]nodeconfig.GroupID{nodeconfig.NewGroupIDByShardID(shard.BeaconChainShardID)},
		p2p.ConstructMessage(
			proto_node.ConstructSlashMessage(slash.Records{*witness})),
	); err != nil {
		utils.Logger().Err(err).
			RawJSON("record", []byte(witness.String())).
			Msg("could not send slash record to beaconchain")
	}
	utils.Logger().Info().Msg("broadcast the double sign record")
}

// BroadcastCrossLink is called by consensus leader to
// send the new header as cross link to beacon chain.
func (node *Node) BroadcastCrossLink() {
	curBlock := node.Blockchain().CurrentBlock()
	if curBlock == nil {
		return
	}
	// uhhBot do not send too much crossLink
	if curBlock.NumberU64() >= 5 {
		return
	}
	if node.IsRunningBeaconChain() ||
		!node.Blockchain().Config().IsCrossLink(curBlock.Epoch()) {
		// no need to broadcast crosslink if it's beacon chain or it's not crosslink epoch
		return
	}

	// no point to broadcast the crosslink if we aren't even in the right epoch yet
	if !node.Blockchain().Config().IsCrossLink(
		node.Blockchain().CurrentHeader().Epoch(),
	) {
		return
	}

	utils.Logger().Info().Msgf(
		"Construct and Broadcasting new crosslink to beacon chain groupID %s",
		nodeconfig.NewGroupIDByShardID(shard.BeaconChainShardID),
	)
	headers := []*block.Header{}
	lastLink, err := node.Beaconchain().ReadShardLastCrossLink(curBlock.ShardID())
	var latestBlockNum uint64

	// TODO chao: record the missing crosslink in local database instead of using latest crosslink
	// if cannot find latest crosslink, broadcast latest 3 block headers
	if err != nil {
		utils.Logger().Debug().Err(err).Msg("[BroadcastCrossLink] ReadShardLastCrossLink Failed")
		header := node.Blockchain().GetHeaderByNumber(curBlock.NumberU64() - 2)
		if header != nil && node.Blockchain().Config().IsCrossLink(header.Epoch()) {
			headers = append(headers, header)
		}
		header = node.Blockchain().GetHeaderByNumber(curBlock.NumberU64() - 1)
		if header != nil && node.Blockchain().Config().IsCrossLink(header.Epoch()) {
			headers = append(headers, header)
		}
		headers = append(headers, curBlock.Header())
	} else {
		latestBlockNum = lastLink.BlockNum()
		if latestSentCrosslinkNum > latestBlockNum && latestSentCrosslinkNum <= latestBlockNum+crossLinkBatchSize*6 {
			latestBlockNum = latestSentCrosslinkNum
		}

		batchSize := crossLinkBatchSize
		diff := curBlock.Number().Uint64() - latestBlockNum

		// Increase batch size by 1 for every 5 blocks behind
		batchSize += int(diff) / 5

		// Cap at a sane size to avoid overload network
		if batchSize > crossLinkBatchSize*2 {
			batchSize = crossLinkBatchSize * 2
		}

		for blockNum := latestBlockNum + 1; blockNum <= curBlock.NumberU64(); blockNum++ {
			header := node.Blockchain().GetHeaderByNumber(blockNum)
			if header != nil && node.Blockchain().Config().IsCrossLink(header.Epoch()) {
				headers = append(headers, header)
				if len(headers) == batchSize {
					break
				}
			}
		}
	}

	utils.Logger().Info().Msgf("[BroadcastCrossLink] Broadcasting Block Headers, latestBlockNum %d, currentBlockNum %d, Number of Headers %d", latestBlockNum, curBlock.NumberU64(), len(headers))
	for i, header := range headers {
		utils.Logger().Debug().Msgf(
			"[BroadcastCrossLink] Broadcasting %d",
			header.Number().Uint64(),
		)
		if i == len(headers)-1 {
			latestSentCrosslinkNum = header.Number().Uint64()
		}
	}
	node.host.SendMessageToGroups(
		[]nodeconfig.GroupID{nodeconfig.NewGroupIDByShardID(shard.BeaconChainShardID)},
		p2p.ConstructMessage(
			proto_node.ConstructCrossLinkMessage(node.Consensus.Blockchain, headers)),
	)
}

// VerifyNewBlock is called by consensus participants to verify the block (account model) they are
// running consensus on
func (node *Node) VerifyNewBlock(newBlock *types.Block) error {
	if newBlock == nil || newBlock.Header() == nil {
		return errors.New("nil header or block asked to verify")
	}

	if newBlock.ShardID() != node.Blockchain().ShardID() {
		utils.Logger().Error().
			Uint32("my shard ID", node.Blockchain().ShardID()).
			Uint32("new block's shard ID", newBlock.ShardID()).
			Msg("[VerifyNewBlock] Wrong shard ID of the new block")
		return errors.New("[VerifyNewBlock] Wrong shard ID of the new block")
	}

	if newBlock.NumberU64() <= node.Blockchain().CurrentBlock().NumberU64() {
		return errors.Errorf("block with the same block number is already committed: %d", newBlock.NumberU64())
	}
	if err := node.Blockchain().Validator().ValidateHeader(newBlock, true); err != nil {
		utils.Logger().Error().
			Str("blockHash", newBlock.Hash().Hex()).
			Err(err).
			Msg("[VerifyNewBlock] Cannot validate header for the new block")
		return err
	}

	if err := node.Blockchain().Engine().VerifyVRF(
		node.Blockchain(), newBlock.Header(),
	); err != nil {
		utils.Logger().Error().
			Str("blockHash", newBlock.Hash().Hex()).
			Err(err).
			Msg("[VerifyNewBlock] Cannot verify vrf for the new block")
		return errors.Wrap(err,
			"[VerifyNewBlock] Cannot verify vrf for the new block",
		)
	}

	if err := node.Blockchain().Engine().VerifyShardState(
		node.Blockchain(), node.Beaconchain(), newBlock.Header(),
	); err != nil {
		utils.Logger().Error().
			Str("blockHash", newBlock.Hash().Hex()).
			Err(err).
			Msg("[VerifyNewBlock] Cannot verify shard state for the new block")
		return errors.Wrap(err,
			"[VerifyNewBlock] Cannot verify shard state for the new block",
		)
	}

	if err := node.Blockchain().ValidateNewBlock(newBlock); err != nil {
		if hooks := node.NodeConfig.WebHooks.Hooks; hooks != nil {
			if p := hooks.ProtocolIssues; p != nil {
				url := p.OnCannotCommit
				go func() {
					webhooks.DoPost(url, map[string]interface{}{
						"bad-header": newBlock.Header(),
						"reason":     err.Error(),
					})
				}()
			}
		}
		utils.Logger().Error().
			Str("blockHash", newBlock.Hash().Hex()).
			Int("numTx", len(newBlock.Transactions())).
			Int("numStakingTx", len(newBlock.StakingTransactions())).
			Err(err).
			Msg("[VerifyNewBlock] Cannot Verify New Block!!!")
		return errors.Errorf(
			"[VerifyNewBlock] Cannot Verify New Block!!! block-hash %s txn-count %d",
			newBlock.Hash().Hex(),
			len(newBlock.Transactions()),
		)
	}

	// Verify cross links
	// TODO: move into ValidateNewBlock
	if node.IsRunningBeaconChain() {
		err := node.VerifyBlockCrossLinks(newBlock)
		if err != nil {
			utils.Logger().Debug().Err(err).Msg("ops2 VerifyBlockCrossLinks Failed")
			return err
		}
	}

	// TODO: move into ValidateNewBlock
	if err := node.verifyIncomingReceipts(newBlock); err != nil {
		utils.Logger().Error().
			Str("blockHash", newBlock.Hash().Hex()).
			Int("numIncomingReceipts", len(newBlock.IncomingReceipts())).
			Err(err).
			Msg("[VerifyNewBlock] Cannot ValidateNewBlock")
		return errors.Wrapf(
			err, "[VerifyNewBlock] Cannot ValidateNewBlock",
		)
	}
	return nil
}

//写一个handle对于horizontal的
func (node *Node) AddOneMoreGroup(topicNameuhhBot nodeconfig.GroupID) error {
	node.psCtx, node.psCancel = context.WithCancel(context.Background())

	// groupID and whether this topic is used for consensus
	type t struct {
		tp    nodeconfig.GroupID
		isCon bool
	}
	groups := map[nodeconfig.GroupID]bool{}

	// three topic subscribed by each validator
	for _, t := range []t{
		{topicNameuhhBot, true},
	} {
		if _, ok := groups[t.tp]; !ok {
			groups[t.tp] = t.isCon
		}
	}

	type u struct {
		p2p.NamedTopic
		consensusBound bool
	}

	var allTopics []u

	utils.Logger().Debug().
		Interface("topics-ended-up-with", groups).
		Msg("uhhBot starting with these topics")

	if !node.NodeConfig.IsOffline {
		for key, isCon := range groups {
			topicHandle, err := node.host.GetOrJoin(string(key))
			if err != nil {
				return err
			}
			allTopics = append(
				allTopics, u{
					NamedTopic:     p2p.NamedTopic{string(key), topicHandle},
					consensusBound: isCon,
				},
			)
		}
	}
	pubsub := node.host.PubSub()
	ownID := node.host.GetID()
	errChan := make(chan withError, 100)

	// p2p consensus message handler function
	type p2pHandlerConsensus func(
		ctx context.Context,
		msg *msg_pb.Message,
		key *bls.SerializedPublicKey,
		divisionFlag *bool,
	) error

	// other p2p message handler function
	type p2pHandlerElse func(
		ctx context.Context,
		rlpPayload []byte,
		actionType proto_node.MessageType,
	) error

	// interface pass to p2p message validator
	type validated struct {
		consensusBound bool
		// 这儿要改
		handleC      p2pHandlerConsensus
		handleCArg   *msg_pb.Message
		handleE      p2pHandlerElse
		handleEArg   []byte
		senderPubKey *bls.SerializedPublicKey
		actionType   proto_node.MessageType
	}

	nodeStringCounterVec.WithLabelValues("peerid", nodeconfig.GetPeerID().String()).Inc()

	for i := range allTopics {
		sub, err := allTopics[i].Topic.Subscribe()
		if err != nil {
			return err
		}

		topicNamed := allTopics[i].Name
		isConsensusBound := allTopics[i].consensusBound

		utils.Logger().Info().
			Str("topic", topicNamed).
			Msg("enabled topic validation pubsub messages")

		// register topic validator for each topic
		if err := pubsub.RegisterTopicValidator(
			topicNamed,
			// this is the validation function called to quickly validate every p2p message
			func(ctx context.Context, peer libp2p_peer.ID, msg *libp2p_pubsub.Message) libp2p_pubsub.ValidationResult {
				nodeP2PMessageCounterVec.With(prometheus.Labels{"type": "total"}).Inc()
				hmyMsg := msg.GetData()

				// keep the log of receiving horizontal message
				if find := strings.Contains(topicNamed, "Horizontal/shard"); find {
					// utils.Logger().Info().
					// 	Str("topic", topicNamed).
					// 	Msg("validation function called to quickly validate every p2p message")
				}

				// first to validate the size of the p2p message
				if len(hmyMsg) < p2pMsgPrefixSize {
					// TODO (lc): block peers sending empty messages
					nodeP2PMessageCounterVec.With(prometheus.Labels{"type": "invalid_size"}).Inc()
					return libp2p_pubsub.ValidationReject
				}

				// 打一个信号去往handle 函数
				openBox := hmyMsg[p2pMsgPrefixSize:]

				// validate message category
				switch proto.MessageCategory(openBox[proto.MessageCategoryBytes-1]) {
				case proto.Consensus:
					// received consensus message in non-consensus bound topic
					if !isConsensusBound {
						nodeP2PMessageCounterVec.With(prometheus.Labels{"type": "invalid_bound"}).Inc()
						errChan <- withError{
							errors.WithStack(errConsensusMessageOnUnexpectedTopic), msg,
						}
						return libp2p_pubsub.ValidationReject
					}
					nodeP2PMessageCounterVec.With(prometheus.Labels{"type": "consensus_total"}).Inc()

					// validate consensus message
					validMsg, senderPubKey, ignore, err := node.validateShardBoundMessage(
						context.TODO(), openBox[proto.MessageCategoryBytes:],
					)

					if err != nil {
						errChan <- withError{err, msg.GetFrom()}
						return libp2p_pubsub.ValidationReject
					}

					// ignore the further processing of the p2p messages as it is not intended for this node
					if ignore {
						return libp2p_pubsub.ValidationAccept
					}

					msg.ValidatorData = validated{
						consensusBound: true,
						handleC:        node.Consensus.HandleMessageUpdate,
						handleCArg:     validMsg,
						senderPubKey:   senderPubKey,
					}
					return libp2p_pubsub.ValidationAccept

				case proto.Node:
					// node message is almost empty
					if len(openBox) <= p2pNodeMsgPrefixSize {
						nodeP2PMessageCounterVec.With(prometheus.Labels{"type": "invalid_size"}).Inc()
						return libp2p_pubsub.ValidationReject
					}
					nodeP2PMessageCounterVec.With(prometheus.Labels{"type": "node_total"}).Inc()
					validMsg, actionType, err := node.validateNodeMessage(
						context.TODO(), openBox,
					)
					if err != nil {
						switch err {
						case errIgnoreBeaconMsg:
							// ignore the further processing of the ignored messages as it is not intended for this node
							// but propogate the messages to other nodes
							return libp2p_pubsub.ValidationAccept
						default:
							// TODO (lc): block peers sending error messages
							errChan <- withError{err, msg.GetFrom()}
							return libp2p_pubsub.ValidationReject
						}
					}
					msg.ValidatorData = validated{
						consensusBound: false,
						handleE:        node.HandleNodeMessage,
						handleEArg:     validMsg,
						actionType:     actionType,
					}
					return libp2p_pubsub.ValidationAccept

				// 林游改了
				// this case is horizontal message type, which is not set in the common.go(hard-coded here)
				case 5:
					nodeP2PMessageCounterVec.With(prometheus.Labels{"type": "horizontal msg"}).Inc()
					//msg.ValidatorData = validated{
					//	consensusBound: false,
					//	handleE:        node.HandleNodeMessage,
					//	handleEArg:     openBox[2:],
					//	actionType:     proto_node.MessageType(openBox[1]),
					//}
					return libp2p_pubsub.ValidationAccept
				default:
					// ignore garbled messages
					// 林游改了
					// when test horizon, it seems that never enter here
					utils.Logger().Warn().
						Str("topic", topicNamed).Msg("ignore garbled messages")
					nodeP2PMessageCounterVec.With(prometheus.Labels{"type": "ignored"}).Inc()
					return libp2p_pubsub.ValidationReject
				}
			},
			// WithValidatorTimeout is an option that sets a timeout for an (asynchronous) topic validator. By default there is no timeout in asynchronous validators.
			// TODO: Currently this timeout is useless. Verify me.
			libp2p_pubsub.WithValidatorTimeout(250*time.Millisecond),
			// WithValidatorConcurrency set the concurernt validator, default is 1024
			libp2p_pubsub.WithValidatorConcurrency(p2p.SetAsideForConsensus),
			// WithValidatorInline is an option that sets the validation disposition to synchronous:
			// it will be executed inline in validation front-end, without spawning a new goroutine.
			// This is suitable for simple or cpu-bound validators that do not block.
			libp2p_pubsub.WithValidatorInline(true),
		); err != nil {
			return err
		}

		msgChanConsensus := make(chan validated, MsgChanBuffer)

		semNode := semaphore.NewWeighted(p2p.SetAsideOtherwise)
		msgChanNode := make(chan validated, MsgChanBuffer)

		// goroutine to handle node messages
		go func() {
			for {
				select {
				case m := <-msgChanNode:
					ctx, cancel := context.WithTimeout(node.psCtx, 10*time.Second)
					msg := m
					go func() {
						defer cancel()
						if semNode.TryAcquire(1) {
							defer semNode.Release(1)

							if err := msg.handleE(ctx, msg.handleEArg, msg.actionType); err != nil {
								errChan <- withError{err, nil}
							}
						}

						select {
						case <-ctx.Done():
							if errors.Is(ctx.Err(), context.DeadlineExceeded) ||
								errors.Is(ctx.Err(), context.Canceled) {
								utils.Logger().Warn().
									Str("topic", topicNamed).Msg("[context] exceeded node message handler deadline")
							}
							errChan <- withError{errors.WithStack(ctx.Err()), nil}
						default:
							return
						}
					}()
				case <-node.psCtx.Done():
					return
				}
			}
		}()
		go func() {
			for {
				nextMsg, err := sub.Next(node.psCtx)
				if err != nil {
					if err == context.Canceled {
						return
					}
					errChan <- withError{errors.WithStack(err), nil}
					continue
				}

				if nextMsg.GetFrom() == ownID {
					continue
				}

				if validatedMessage, ok := nextMsg.ValidatorData.(validated); ok {
					if validatedMessage.consensusBound {
						msgChanConsensus <- validatedMessage
					} else {
						msgChanNode <- validatedMessage
					}
				} else {
					// continue if ValidatorData is nil
					if nextMsg.ValidatorData == nil {
						continue
					}
				}
			}
		}()

	}

	return nil
}

func (node *Node) PostConsensusProcessing(newBlock *types.Block) error {
	if node.Consensus.IsLeader() {
		if node.IsRunningBeaconChain() {
			// TODO: consider removing this and letting other nodes broadcast new blocks.
			// But need to make sure there is at least 1 node that will do the job.
			node.BroadcastNewBlock(newBlock)
		}
		node.BroadcastCXReceipts(newBlock)

		//处理baseline intra-shard 交易
		for _, item := range newBlock.Header().TransactionMsg() {
			if item.ToShard == item.FromShard {
				utils.Logger().Info().
					Uint32("count", item.Count).
					// confirming transactions
					Str("", "ct").
					Float64("latency", float64(time.Since(time.Unix(newBlock.Header().Time().Int64(), 0)).Seconds())).
					Msg("")
			}
		}

		// 这个区块的交易完成了，所以我要处理对应的交易
		txnsMsg := append([]blockif.TransactionMsg(nil), newBlock.Header().TransactionReceiptMsg()...)
		for index, item := range txnsMsg {
			if !item.IsMeta {
				// baseline cross-shard 第二段
				utils.Logger().Info().
					Uint32("fromShard", item.FromShard).
					Uint32("count", item.Count).
					Str("", "ctx").
					Float64("latency", float64(time.Since(time.Unix(int64(item.BeginTime), 0)).Seconds())).
					Msg("")
			} else {
				// meta的交易
				if item.FromShard == node.Consensus.ShardID && item.ToShard == node.Consensus.ShardID && item.ToMeta == item.FromMeta {
					// 我自己meta的meta内交易
					utils.Logger().Info().
						Uint32("count", item.Count).
						Uint32("ShardID", item.FromShard).
						Uint32("Meta", item.ToMeta).
						Str("", "ctm").
						Float64("latency", float64(time.Since(time.Unix(int64(item.BeginTime), 0)).Seconds())).
						Msg("")
				} else if !item.FromOK {
					// 发送给对应的meta让他们自己处理一下先
					// 我猜是把header发过去啦
					tmpName := nodeconfig.NewGroupIDByHorizontalShardID(nodeconfig.ShardID(item.ToShard),
						node.Consensus.DivisionLevel,
						uint32(item.ToMeta),
					)

					txnsMsg[index].FromOK = true

					metaData := &types.MetaGroupData_syncHeader{
						BlockNum:    uint32(newBlock.Number().Uint64()),
						ShardID:     node.Consensus.ShardID,
						MetaGroupID: item.FromMeta,
						IsCross:     true,
						CrossTxns:   txnsMsg[index : index+1],
					}
					// uhhBottodo 这儿怎么只有full leader在发？实际上应该是这个小meta自个证明给对应的meta
					go func() {
						if err := node.host.SendMessageToGroups([]nodeconfig.GroupID{tmpName},
							p2p.ConstructMessage(proto_node.ConstructMetaHeaderData(metaData)),
						); err != nil {
							utils.Logger().Error().Err(err).
								Str("groupName", tmpName.String()).
								Msg("[sendCross_meta] Cannot Cross")
						} else {
							utils.Logger().Info().
								Str("groupName", tmpName.String()).
								Interface("metaData", metaData).
								Msg("[sendCross_meta] send Cross!! testlin")
						}
					}()
				} else {
					// 跨meta交易的第二步
					utils.Logger().Info().
						Uint32("count", item.Count).
						Uint32("FromShardID", item.FromShard).
						Uint32("FromMeta", item.FromMeta).
						Uint32("ToShardID", item.ToShard).
						Uint32("ToMeta", item.ToMeta).
						Str("", "ctmx").
						Float64("latency", float64(time.Since(time.Unix(int64(item.BeginTime), 0)).Seconds())).
						Msg("")
				}
			}
		}
	} else {
		if node.Consensus.Mode() != consensus.Listening {
			utils.Logger().Info().
				Uint64("blockNum", newBlock.NumberU64()).
				Uint64("epochNum", newBlock.Epoch().Uint64()).
				Uint64("ViewId", newBlock.Header().ViewID().Uint64()).
				Str("blockHash", newBlock.Hash().String()).
				Int("numTxns", len(newBlock.Transactions())).
				Int("numStakingTxns", len(newBlock.StakingTransactions())).
				Uint32("numSignatures", node.Consensus.NumSignaturesIncludedInBlock(newBlock)).
				Msg("BINGO !!! Reached Consensus")

			numSig := float64(node.Consensus.NumSignaturesIncludedInBlock(newBlock))
			node.Consensus.UpdateValidatorMetrics(numSig, float64(newBlock.NumberU64()))

			// 1% of the validator also need to do broadcasting
			// uhhBot test 随机一些节点帮帮忙
			rnd := rand.Intn(40)
			if rnd < 1 {
				// Beacon validators also broadcast new blocks to make sure beacon sync is strong.
				// if node.IsRunningBeaconChain() {
				// 	node.BroadcastNewBlock(newBlock)
				// }
				node.BroadcastCXReceipts(newBlock)

				// 这个区块的交易完成了，所以我要处理对应的交易
				txnsMsg := append([]blockif.TransactionMsg(nil), newBlock.Header().TransactionReceiptMsg()...)
				for index, item := range txnsMsg {
					isIntraMeta := item.FromShard == node.Consensus.ShardID && item.ToShard == node.Consensus.ShardID && item.ToMeta == item.FromMeta

					if item.IsMeta && !isIntraMeta && !item.FromOK {
						// 发送给对应的meta让他们自己处理一下先

						tmpName := nodeconfig.NewGroupIDByHorizontalShardID(nodeconfig.ShardID(item.ToShard),
							node.Consensus.DivisionLevel,
							uint32(item.ToMeta),
						)

						txnsMsg[index].FromOK = true

						metaData := &types.MetaGroupData_syncHeader{
							BlockNum:    uint32(newBlock.Number().Uint64()),
							ShardID:     node.Consensus.ShardID,
							MetaGroupID: item.FromMeta,
							IsCross:     true,
							CrossTxns:   txnsMsg[index : index+1],
						}
						// uhhBottodo 这儿怎么只有full leader在发？实际上应该是这个小meta自个证明给对应的meta
						go func() {
							if err := node.host.SendMessageToGroups([]nodeconfig.GroupID{tmpName},
								p2p.ConstructMessage(proto_node.ConstructMetaHeaderData(metaData)),
							); err != nil {
								utils.Logger().Error().Err(err).
									Str("groupName", tmpName.String()).
									Msg("[sendCross_meta] Cannot Cross")
							} else {
								utils.Logger().Info().
									Str("groupName", tmpName.String()).
									Interface("metaData", metaData).
									Msg("[sendCross_meta] send Cross!! testlin")
							}
						}()

					}
				}

			}
		}
	}

	// Broadcast client requested missing cross shard receipts if there is any
	// node.BroadcastMissingCXReceipts()

	if h := node.NodeConfig.WebHooks.Hooks; h != nil {
		if h.Availability != nil {
			for _, addr := range node.GetAddresses(newBlock.Epoch()) {
				wrapper, err := node.Beaconchain().ReadValidatorInformation(addr)
				if err != nil {
					utils.Logger().Err(err).Str("addr", addr.Hex()).Msg("failed reaching validator info")
					return nil
				}
				snapshot, err := node.Beaconchain().ReadValidatorSnapshot(addr)
				if err != nil {
					utils.Logger().Err(err).Str("addr", addr.Hex()).Msg("failed reaching validator snapshot")
					return nil
				}
				computed := availability.ComputeCurrentSigning(
					snapshot.Validator, wrapper,
				)
				lastBlockOfEpoch := shard.Schedule.EpochLastBlock(node.Beaconchain().CurrentBlock().Header().Epoch().Uint64())

				computed.BlocksLeftInEpoch = lastBlockOfEpoch - node.Beaconchain().CurrentBlock().Header().Number().Uint64()

				if err != nil && computed.IsBelowThreshold {
					url := h.Availability.OnDroppedBelowThreshold
					go func() {
						webhooks.DoPost(url, computed)
					}()
				}
			}
		}
	}
	return nil
}

// BootstrapConsensus is the a goroutine to check number of peers and start the consensus
func (node *Node) BootstrapConsensus() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	min := node.Consensus.MinPeers
	enoughMinPeers := make(chan struct{})
	const checkEvery = 3 * time.Second
	go func() {
		for {
			<-time.After(checkEvery)
			numPeersNow := node.host.GetPeerCount()
			if numPeersNow >= min {
				utils.Logger().Info().Msg("[bootstrap] StartConsensus")
				enoughMinPeers <- struct{}{}
				return
			}
			utils.Logger().Info().
				Int("numPeersNow", numPeersNow).
				Int("targetNumPeers", min).
				Dur("next-peer-count-check-in-seconds", checkEvery).
				Msg("do not have enough min peers yet in bootstrap of consensus")
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-enoughMinPeers:
		go func() {
			node.startConsensus <- struct{}{}
		}()
		return nil
	}
}
