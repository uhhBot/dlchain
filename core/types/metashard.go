package types

import blockif "github.com/harmony-one/harmony/block/interface"

// CXContract represents a cross-shard contract call
// MetaGroupDataStage represents the type of messages used for Node/Block
type MetaGroupDataStage uint32

// Block sync message subtype
const (
	Announce MetaGroupDataStage = iota
	Prepare
	Prepared
	Commit
	Committed
	Bootstrap
	CrossMeta
)

// 共识信息只在自己的meta里面传递
type MetaConsensusData struct {
	BlockNum      uint32
	DivisionLevel uint32
	MetaGroupID   uint32
	Body          []byte
	Stage         MetaGroupDataStage
	Txns          []blockif.TransactionMsg
	CrossTxns     []blockif.TransactionMsg
	GroupSize     uint32
	ConsensusTime uint32
}

// 同步信息发送给meta外部
type MetaGroupData_syncHeader struct {
	BlockNum      uint32
	ShardID       uint32
	DivisionLevel uint32
	MetaGroupID   uint32
	Body          []byte
	Txns          []blockif.TransactionMsg
	CrossTxns     []blockif.TransactionMsg
	IsCross       bool
	ConsensusTime uint32
}

// CXContract represents a cross-shard contract call
type CXContract struct {
	BlockNum uint64
	Step     uint32
	Shard0   uint32
	Shard1   uint32
	Body     []byte
	TotalNum uint64
	SelfNum  uint64
}
