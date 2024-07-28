package node

import (
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
	proto_node "github.com/harmony-one/harmony/api/proto/node"
	blockif "github.com/harmony-one/harmony/block/interface"
	"github.com/harmony-one/harmony/core/types"
	nodeconfig "github.com/harmony-one/harmony/internal/configs/node"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/p2p"
	"github.com/harmony-one/harmony/shard"
)

var BlockNum uint32 = 0
var DivisionLevel uint32 = 1
var MetaGroupID uint32 = 0
var Stage types.MetaGroupDataStage
var IsLeader = false
var LastTime = time.Now()
var consensusTime = uint32(time.Now().Unix())
var VoteCnt uint32 = 0
var GroupSize uint32 = 0

var CrossMsgForNextBlock []blockif.TransactionMsg
var CrossSeenMap = make(map[string]bool)

var mutex sync.Mutex

// 处理同步信息,来自于本分片的meta或者其他full shard 已经处理了一半的跨分片交易
func (node *Node) ProcessMetaHeaderMsg(msgPayload []byte) {

	// 预处理数据
	msg := types.MetaGroupData_syncHeader{}
	if err := rlp.DecodeBytes(msgPayload, &msg); err != nil {
		utils.Logger().Error().Err(err).
			Msg("[ProcessMetaHeaderMsg] Unable to Decode message Payload")
		return
	}
	node.Consensus.NewTxnsMutex.Lock()
	defer node.Consensus.NewTxnsMutex.Unlock()

	// 如果我是本大分片leader，修改已知的内部meta信息
	// fix 大分片的leader不希望在此处理其他节点帮他发的from meta ok的证明
	if !msg.IsCross && node.Consensus.IsLeader() && msg.ShardID == node.Consensus.ShardID && msg.BlockNum > node.Consensus.MetaStage[msg.DivisionLevel].BlockID[msg.MetaGroupID] {
		node.Consensus.MetaStage[msg.DivisionLevel].BlockID[msg.MetaGroupID] = msg.BlockNum
		// 林游TODO：记录一下meta达成的交易

		// 把我的meta的交易当做跨分片事务来处理
		node.Consensus.NewTxnsForNextBlock = append(node.Consensus.NewTxnsForNextBlock, msg.Txns...)
		node.Consensus.NewTxnsForNextBlock = append(node.Consensus.NewTxnsForNextBlock, msg.CrossTxns...)

		utils.Logger().Info().
			Uint32("Level", msg.DivisionLevel).
			Uint32("GroupID", msg.MetaGroupID).
			Uint32("BlockID", msg.BlockNum).
			Float64("ConsensusTimeMeta", float64(msg.ConsensusTime)/1000).
			// Interface("Txns", msg.Txns).
			// Interface("NewTxnsForNextBlock", node.Consensus.NewTxnsForNextBlock).
			Msg("[ProcessMetaHeaderMsg] update meta blockheader")
	} else if node.Consensus.IsMetaLeader && msg.IsCross {
		// 如果我是meta的leader,那么可能需要处理之前的跨分片交易的后半段
		// 唯一键 ：fromshard__frommeta__metablockID
		key := "fromOK" + strconv.Itoa(int(msg.ShardID)) + strconv.Itoa(int(msg.MetaGroupID)) + strconv.Itoa(int(msg.BlockNum))

		_, ok := CrossSeenMap[key]
		if ok {
			return
		} else {
			CrossSeenMap[key] = true
			// 当做跨分片事务来处理
			CrossMsgForNextBlock = append(CrossMsgForNextBlock, msg.CrossTxns...)
		}
		utils.Logger().Info().
			// Str("groupName", tmpName.String()).
			Msg("testlin from已经共识完成")
	}
}

func (node *Node) ProcessMetaConsensusMsg(msgPayload []byte) {

	// 预处理数据
	msg := types.MetaConsensusData{}
	if err := rlp.DecodeBytes(msgPayload, &msg); err != nil {
		utils.Logger().Error().Err(err).
			Msg("[ProcessMetaConsensusMsg] Unable to Decode message Payload")
		return
	}
	utils.Logger().Info().
		Msg("[ProcessMetaConsensusMsg] get one message")
	// 更改状态
	intendedForLeader := node.Consensus.IsMetaLeader
	intendedForValidator := !intendedForLeader
	IsLeader = node.Consensus.IsMetaLeader

	// Route message to handler
	switch t := msg.Stage; true {
	// Handle validator intended messages first
	case t == types.Announce && intendedForValidator:
		onAnnounce_meta(&msg, node)
	case t == types.Prepared && intendedForValidator:
		onPrepared_meta(&msg, node)
	case t == types.Committed && intendedForValidator:
		onCommitted_meta(&msg, node)

	// Handle leader intended messages now
	case t == types.Prepare && intendedForLeader:
		onPrepare_meta(&msg, node)
	case t == types.Commit && intendedForLeader:
		onCommit_meta(&msg, node)
	}
}

// leader
func (node *Node) SendAnnounce_meta() {

	//// Lock Write - Start
	mutex.Lock()
	BlockNum += 1
	DivisionLevel = uint32(node.Consensus.DivisionLevel)
	MetaGroupID = node.Consensus.MetaGroupID
	Stage = types.Announce
	VoteCnt = 1

	GroupSize = uint32(node.Consensus.MetaGroupSize)

	// 构建交易内容#######
	ins := shard.Schedule.InstanceForEpoch(common.Big0)
	// 有一个beacon
	numMeta := (ins.NumShards() - 1) * DivisionLevel

	tmpTXNS := make([]blockif.TransactionMsg, 0)
	// 临时 动态小分片负载
	// TOTALNUMX := uint32(time.Since(LastTime).Seconds() * 136)
	LastTime = time.Now()
	consensusTime = uint32(time.Now().Unix())
	TOTALNUMX := uint32(4096)
	for sid := 1; sid < int(ins.NumShards()); sid++ {
		for mid := 0; mid < int(DivisionLevel); mid++ {

			tmpTXNS = append(tmpTXNS, blockif.TransactionMsg{
				FromShard: node.Consensus.ShardID,
				ToShard:   uint32(sid),
				IsMeta:    true,
				FromMeta:  MetaGroupID,
				ToMeta:    uint32(mid),
				Count:     uint32(TOTALNUMX / numMeta),
				BeginTime: uint32(time.Now().Unix()),
			})
		}
	}
	// 构建交易内容#######

	body := make([]byte, 1024*1024*2)
	metaData := &types.MetaConsensusData{
		BlockNum:      BlockNum,
		DivisionLevel: DivisionLevel,
		MetaGroupID:   MetaGroupID,
		Body:          body,
		Stage:         types.Announce,
		Txns:          tmpTXNS,
		CrossTxns:     CrossMsgForNextBlock,
		GroupSize:     GroupSize,
	}
	CrossMsgForNextBlock = nil
	mutex.Unlock()
	//// Lock Write - End

	if err := node.host.SendMessageToGroups([]nodeconfig.GroupID{nodeconfig.GroupID(node.Consensus.MetaGroupName)},
		p2p.ConstructMessage(proto_node.ConstructMetaConsensusData(metaData)),
	); err != nil {
		utils.Logger().Error().Err(err).
			Uint32("groupID", MetaGroupID).
			Msg("[SendAnnounce_meta] Cannot announce")
	} else {
		utils.Logger().Info().
			Uint32("groupID", MetaGroupID).
			Uint32("BlockNum", metaData.BlockNum).
			Uint32("DivisionLevel", metaData.DivisionLevel).
			Uint32("MetaGroupID", metaData.MetaGroupID).
			Interface("Txns", metaData.Txns).
			Interface("CrossTxns", metaData.CrossTxns).
			Msg("[sendAnnounce_meta] send announce!!")
	}

}

func onAnnounce_meta(msg *types.MetaConsensusData, node *Node) {

	utils.Logger().Info().
		Uint64("MsgLevel", uint64(msg.DivisionLevel)).
		Uint64("MsgBlockNum", uint64(msg.BlockNum)).
		Msg("[onAnnounce_meta] Announce message Added")

	body := make([]byte, 199)

	mutex.Lock()
	BlockNum = msg.BlockNum
	DivisionLevel = msg.DivisionLevel
	MetaGroupID = msg.MetaGroupID
	Stage = types.Announce

	VoteCnt = 1
	GroupSize = msg.GroupSize

	// send prepared
	metaData := &types.MetaConsensusData{
		BlockNum:      BlockNum,
		DivisionLevel: DivisionLevel,
		MetaGroupID:   MetaGroupID,
		Body:          body,
		Stage:         types.Prepare,
		Txns:          msg.Txns,
		CrossTxns:     msg.CrossTxns,
	}
	mutex.Unlock()

	if err := node.host.SendMessageToGroups([]nodeconfig.GroupID{nodeconfig.GroupID(node.Consensus.MetaGroupName)},
		p2p.ConstructMessage(proto_node.ConstructMetaConsensusData(metaData)),
	); err != nil {
		utils.Logger().Error().Err(err).
			Uint32("groupID", MetaGroupID).
			Msg("[sendPrepare_meta] Cannot prepare")
	} else {
		utils.Logger().Info().
			Uint32("groupID", MetaGroupID).
			Msg("[sendPrepare_meta] send prepare!!")
	}
}

// leader
func onPrepare_meta(msg *types.MetaConsensusData, node *Node) {

	mutex.Lock()
	if msg.BlockNum != BlockNum ||
		msg.DivisionLevel != DivisionLevel ||
		msg.MetaGroupID != MetaGroupID ||
		Stage != types.Announce {
		utils.Logger().Info().
			Msg("[warning_onPrepare_meta] not my prepare")
		mutex.Unlock()
		return
	}

	VoteCnt += 1
	// uhhBot test 临时恶意比例
	if VoteCnt <= GroupSize/2 {
		utils.Logger().Info().Uint32("VoteCnt", VoteCnt).Uint32("GroupSize", GroupSize).
			Msg("[OnPrepare_meta] Received Prepare_meta")
		mutex.Unlock()

	} else {
		//
		// 进入到prepared
		utils.Logger().Info().Uint32("VoteCnt", VoteCnt).Uint32("GroupSize", GroupSize).
			Msg("[OnPrepare_meta] Received enough Prepare_meta!!")

		Stage = types.Prepared
		VoteCnt = 1

		body := make([]byte, 2048)
		metaData := &types.MetaConsensusData{
			BlockNum:      BlockNum,
			DivisionLevel: DivisionLevel,
			MetaGroupID:   MetaGroupID,
			Body:          body,
			Stage:         types.Prepared,
			Txns:          msg.Txns,
			CrossTxns:     msg.CrossTxns,
		}
		mutex.Unlock()

		if err := node.host.SendMessageToGroups([]nodeconfig.GroupID{nodeconfig.GroupID(node.Consensus.MetaGroupName)},
			p2p.ConstructMessage(proto_node.ConstructMetaConsensusData(metaData)),
		); err != nil {
			utils.Logger().Error().Err(err).
				Uint32("groupID", MetaGroupID).
				Msg("[sendPrepared_meta] Cannot send prepared message")
		} else {
			utils.Logger().Info().
				Uint32("groupID", MetaGroupID).
				Msg("[sendPrepared_meta] send prepared!!")
		}
	}

}

func onPrepared_meta(msg *types.MetaConsensusData, node *Node) {

	utils.Logger().Info().
		Uint64("MsgLevel", uint64(msg.DivisionLevel)).
		Uint64("MsgBlockNum", uint64(msg.BlockNum)).
		Msg("[onPrepared_meta] prepared message Added")

	body := make([]byte, 199)

	mutex.Lock()
	BlockNum = msg.BlockNum
	DivisionLevel = msg.DivisionLevel
	MetaGroupID = msg.MetaGroupID
	Stage = types.Prepared

	VoteCnt = 1
	GroupSize = msg.GroupSize

	// send prepared
	metaData := &types.MetaConsensusData{
		BlockNum:      BlockNum,
		DivisionLevel: DivisionLevel,
		MetaGroupID:   MetaGroupID,
		Body:          body,
		Stage:         types.Commit,
		Txns:          msg.Txns,
		CrossTxns:     msg.CrossTxns,
	}
	mutex.Unlock()

	if err := node.host.SendMessageToGroups([]nodeconfig.GroupID{nodeconfig.GroupID(node.Consensus.MetaGroupName)},
		p2p.ConstructMessage(proto_node.ConstructMetaConsensusData(metaData)),
	); err != nil {
		utils.Logger().Error().Err(err).
			Uint32("groupID", MetaGroupID).
			Msg("[sendCommit_meta] Cannot commit")
	} else {
		utils.Logger().Info().
			Uint32("groupID", MetaGroupID).
			Msg("[sendCommit_meta] send commit!!")
	}

}

// leader
func onCommit_meta(msg *types.MetaConsensusData, node *Node) {
	//
	mutex.Lock()
	utils.Logger().Info().
		Msg("[Commit_meta_debug] Received commit")

	if msg.DivisionLevel != DivisionLevel ||
		msg.BlockNum != BlockNum ||
		msg.MetaGroupID != MetaGroupID ||
		Stage != types.Prepared {

		mutex.Unlock()
		utils.Logger().Info().
			Msg("[warning_onCommit_meta] Received not my commit")
		return
	}

	//// Read - Start

	VoteCnt += 1

	if VoteCnt <= GroupSize/2 {
		utils.Logger().Info().Uint32("VoteCnt", VoteCnt).
			Msg("[OnCommit_meta] Received Commit_meta")
		mutex.Unlock()
	} else {

		// 票收齐了
		utils.Logger().Info().Uint32("VoteCnt", VoteCnt).
			Msg("[OnCommit_meta] Received enough Commit_meta, sendcommited")

		Stage = types.Committed

		// 本meta共识已经完成，直接发送跨分片
		// 跨分片事务的负载需要发送给对应的metaGroup
		// body := make([]byte, 80*msg.Txns[1].Count)
		consensusCostTime := time.Since(time.Unix(int64(consensusTime), 0)).Seconds() * 1000

		utils.Logger().Info().Str("uhhBot you", "testing").
			Float64("time1", consensusCostTime/1000).
			Msg("[OnCommit_meta] Received enough Commit_meta, sendcommited")
		metaData := &types.MetaConsensusData{
			BlockNum:      BlockNum,
			DivisionLevel: DivisionLevel,
			MetaGroupID:   MetaGroupID,
			// Body:          body,
			Stage:         types.Committed,
			Txns:          msg.Txns,
			CrossTxns:     msg.CrossTxns,
			ConsensusTime: uint32(consensusCostTime),
		}

		payload := make([]byte, 838)
		headerPayload := &types.MetaGroupData_syncHeader{
			BlockNum:      BlockNum,
			ShardID:       node.Consensus.ShardID,
			DivisionLevel: DivisionLevel,
			MetaGroupID:   MetaGroupID,
			Body:          payload,
			Txns:          msg.Txns,
			CrossTxns:     msg.CrossTxns,
			ConsensusTime: uint32(consensusCostTime),
		}

		// 更新本地的MetaStage
		// 如果shard leader == meta leader 那么这里不能注释！
		// node.Consensus.MetaStage[DivisionLevel].BlockID[MetaGroupID] = BlockNum

		mutex.Unlock()
		// 发送提交信息给自己的分片
		go func() {
			if err := node.host.SendMessageToGroups([]nodeconfig.GroupID{nodeconfig.GroupID(node.Consensus.MetaGroupName)},
				p2p.ConstructMessage(proto_node.ConstructMetaConsensusData(metaData)),
			); err != nil {
				utils.Logger().Error().Err(err).
					Uint32("groupID", MetaGroupID).
					Msg("[sendCommitted_meta] Cannot send Committed message")
			} else {
				utils.Logger().Info().
					Uint32("groupID", MetaGroupID).
					Uint64("MsgLevel", uint64(msg.DivisionLevel)).
					Uint64("MsgBlockNum", uint64(msg.BlockNum)).
					// Uint64("NumberOfTX", uint64(msg.NumberOfTX)).
					Str("", "[sendCommitted_meta]").
					Float64("ConsensusTime", consensusCostTime/1000).
					Msg("")
			}
		}()

		// 补一个header信息给大分片
		go func() {
			// 同步一下metablock header

			// 负载丢过去

			if err := node.host.SendMessageToGroups([]nodeconfig.GroupID{
				nodeconfig.NewGroupIDByShardID(nodeconfig.ShardID(node.Consensus.ShardID)),
			},
				p2p.ConstructMessage(proto_node.ConstructMetaHeaderData(headerPayload)),
			); err != nil {
				utils.Logger().Error().Err(err).
					Uint32("ShardId", (node.Consensus.ShardID)).
					Msg("broadcast meta block header")
			} else {
				utils.Logger().Info().
					Uint32("ShardId", (node.Consensus.ShardID)).
					Msg("broadcast meta block header")
			}
		}()
		time.Sleep(time.Duration(node.Consensus.WaitTimeMeta) * time.Second)
		node.SendAnnounce_meta()

	}

}

func onCommitted_meta(msg *types.MetaConsensusData, node *Node) {
	// 一的meta节点发送区块给大共识组
	// uhhBot test
	if rand.Intn(10) == 0 {
		// 发送给full shard
		// uhhBot test 临时
		payload := make([]byte, 1)
		headerPayload := &types.MetaGroupData_syncHeader{
			BlockNum:      msg.BlockNum,
			ShardID:       node.Consensus.ShardID,
			DivisionLevel: msg.DivisionLevel,
			MetaGroupID:   msg.MetaGroupID,
			Body:          payload,
			Txns:          msg.Txns,
			CrossTxns:     msg.CrossTxns,
			ConsensusTime: msg.ConsensusTime,
		}
		if err := node.host.SendMessageToGroups([]nodeconfig.GroupID{
			nodeconfig.NewGroupIDByShardID(nodeconfig.ShardID(node.Consensus.ShardID)),
		},
			p2p.ConstructMessage(proto_node.ConstructMetaHeaderData(headerPayload)),
		); err != nil {
			utils.Logger().Error().Err(err).
				Uint32("ShardId", (node.Consensus.ShardID)).
				Msg("broadcast meta block header")
		} else {
			utils.Logger().Info().
				Uint32("ShardId", (node.Consensus.ShardID)).
				Msg("broadcast meta block header")
		}
		// 发送给跨分片事务

	}
	utils.Logger().Info().
		Uint64("MsgLevel", uint64(msg.DivisionLevel)).
		Uint64("MsgBlockNum", uint64(msg.BlockNum)).
		// Uint64("NumberOfTX", uint64(msg.NumberOfTX)).
		Msg("[onCommitted_meta] add new meta block!!")
}

func (node *Node) ProcessCXContractMessageDIYLattice(msgPayload []byte) {
	utils.Logger().Info().
		Msg("[ProcessCXContractMessageDIYLattice] uhhBot cnmd")
	cxp := types.CXContract{}
	if err := rlp.DecodeBytes(msgPayload, &cxp); err != nil {
		utils.Logger().Error().Err(err).
			Msg("[ProcessCXContractMessageDIYLattice] Unable to Decode message Payload")
		return
	}

}
