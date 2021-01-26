// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"go.etcd.io/etcd/v3/etcdserver/api/rafthttp"
	"go.etcd.io/etcd/v3/etcdserver/api/snap"
	stats "go.etcd.io/etcd/v3/etcdserver/api/v2stats"
	"go.etcd.io/etcd/v3/pkg/fileutil"
	"go.etcd.io/etcd/v3/pkg/types"
	"go.etcd.io/etcd/v3/raft"
	"go.etcd.io/etcd/v3/raft/raftpb"
	"go.etcd.io/etcd/v3/wal"
	"go.etcd.io/etcd/v3/wal/walpb"

	"go.uber.org/zap"
)

// A key-value stream backed by raft Raft 支持的KV stream
type raftNode struct {
	proposeC    <-chan string            // proposed messages (k,v) ，接收PUT 信号
	confChangeC <-chan raftpb.ConfChange // proposed cluster config changes  接收 POST　信号
	commitC     chan<- *string           // entries committed to log (k,v) 接收已提交的条目信号
	errorC      chan<- error             // errors from raft session，接收错误信号

	id          int      // client ID for raft session，当前节点ID
	peers       []string // raft peer URLs，当前集群所有节点的地址
	join        bool     // node is joining an existing cluster，当前节点是否为后续加入到一个集群的节点
	waldir      string   // path to WAL directory，存当 WAL 日志文件的地址
	snapdir     string   // path to snapshot directory。存放快照的目录
	getSnapshot func() ([]byte, error)// 用于获取快照数据的函数
	lastIndex   uint64 // index of log at start // 当回放 WAL 日志结束后，会使用该字段记录最后以一条 Entry 记录的索引值

	confState     raftpb.ConfState // 用于记录当前的集群状态
	snapshotIndex uint64 // 保存当前快照的相关元素，即快照所包含的最后一条 Entry 记录的索引值
	appliedIndex  uint64 // 保存上层模块已应用的位置，即已应用的最后一条 Entry 记录的索引值

	// raft backing for the commit/error channel
	node        raft.Node // 即底层 <b>Raft协议<b>组件，raftNode可以通过node提供的接口来与Raft组件进行交互。
	raftStorage *raft.MemoryStorage //  Raft协议的状态存储组件，应用在更新kvStore状态机时，也会更新此组件，并且通过raft.Config传给Raft协议。
	wal         *wal.WAL // 负责 WAL 日志的管理

	snapshotter      *snap.Snapshotter // 主要用于初始化的过程中监听 snapshotter 实例是否创建完成，snapshotter 负责管理 etcd-raft 模块新产生的快照数据
	snapshotterReady chan *snap.Snapshotter // signals when snapshotter is ready 该通道用于通知上层模块 snapshotter 实例是否已经创建完成

	snapCount uint64
	transport *rafthttp.Transport // 应用通过此接口与集群中其它的节点(peer)通信，比如传输日志同步消息、快照同步消息等。网络传输也是由应用来处理。
	stopc     chan struct{} // signals proposal channel closed
	httpstopc chan struct{} // signals http server to shutdown
	httpdonec chan struct{} // signals http server shutdown complete 这两个通道相互协作，完成当前节点的关闭工作，两者的工作方式与前面介绍的 node.done 和 node.stop 的工作方式类似
}

var defaultSnapshotCount uint64 = 10000

// newRaftNode initiates a raft instance and returns a committed log entry
// channel and error channel. Proposals for log updates are sent over the
// provided the proposal channel. All log entries are replayed over the
// commit channel, followed by a nil message (to indicate the channel is
// current), then new log entries. To shutdown, close proposeC and read errorC.
// 项目的核心部分之一，负责了从node、httpserver、kvs等模块的各个监听事件，并把对应模块进行了串联操作，这里用到了raftNode
func newRaftNode(id int, peers []string, join bool, getSnapshot func() ([]byte, error), proposeC <-chan string,
	confChangeC <-chan raftpb.ConfChange) (<-chan *string, <-chan error, <-chan *snap.Snapshotter) {

	commitC := make(chan *string)
	errorC := make(chan error)

	rc := &raftNode{
		proposeC:    proposeC, // 接收 put 信号
		confChangeC: confChangeC, // 接收 post 信号
		commitC:     commitC, // 接收已经完成的提交的条目
		errorC:      errorC, // 接收异常信号
		id:          id, // 当前节点ID
		peers:       peers, // 所有节点信息
		join:        join, // 当前节点是否为后续加入到一个集群的节点
		waldir:      fmt.Sprintf("raftexample-%d", id),
		snapdir:     fmt.Sprintf("raftexample-%d-snap", id),
		getSnapshot: getSnapshot,
		snapCount:   defaultSnapshotCount, // 只有当日志数量达到此阈值时才执行快照
		stopc:       make(chan struct{}),
		httpstopc:   make(chan struct{}),
		httpdonec:   make(chan struct{}),

		snapshotterReady: make(chan *snap.Snapshotter, 1),
		// rest of structure populated after WAL replay
	}
	go rc.startRaft() // 启动 Raft
	return commitC, errorC, rc.snapshotterReady
}
// 处理 Ready 实例中封装的各项数据的过程
func (rc *raftNode) saveSnap(snap raftpb.Snapshot) error {
	// must save the snapshot index to the WAL before saving the
	// snapshot to maintain the invariant that we only Open the
	// wal at previously-saved snapshot indexes.
	walSnap := walpb.Snapshot{ // 根据快照的元数据，创建 walpb.Snapshot 实例
		Index: snap.Metadata.Index,
		Term:  snap.Metadata.Term,
	}
	if err := rc.wal.SaveSnapshot(walSnap); err != nil {
		return err
	}  // WAL 会将上述快照的元数据信息封装成一条日志记录下来， WAL 的实现在后面的章节中详细介绍
	if err := rc.snapshotter.SaveSnap(snap); err != nil {
		return err
	}     //  将新快照数据写入快照文件中
	return rc.wal.ReleaseLockTo(snap.Metadata.Index) // 根据快照的元数据信息，释放一些无用的 WAL 日志文件的句柄， WAL 的实现在后面的章节中详细介绍
}
// 看一下 raftNode 是如何处理待应用的 Entry 记录的
func (rc *raftNode) entriesToApply(ents []raftpb.Entry) (nents []raftpb.Entry) {
	if len(ents) == 0 {
		return ents
	}   // 长度检测
	firstIdx := ents[0].Index
	if firstIdx > rc.appliedIndex+1 { // 检测 firstIndex 是否合法
		log.Fatalf("first index of committed entry[%d] should <= progress.appliedIndex[%d]+1", firstIdx, rc.appliedIndex)
	}
	if rc.appliedIndex-firstIdx+1 < uint64(len(ents)) { // 过滤掉已经被应用过的 Entry 记录
		nents = ents[rc.appliedIndex-firstIdx+1:]
	}
	return nents
}

// publishEntries writes committed log entries to commit channel and returns
// whether all entries could be published. 在该方法中， raftNode 会将所有待应用记录写入 cornrnitC 通道中。后续 kvstore 就可以读取 cornrnitC 通道并保存相应的键值对数据
func (rc *raftNode) publishEntries(ents []raftpb.Entry) bool {
	for i := range ents {
		switch ents[i].Type {
		case raftpb.EntryNormal:
			if len(ents[i].Data) == 0 {  // 如果 Entry 记录的 Data 为空 ， 则直接忽略该条 Entry 记录
				// ignore empty messages
				break
			}
			s := string(ents[i].Data)
			select {
			case rc.commitC <- &s: // 将数据写入 commitC 通道， kvstore 会读取从其中读取并记录相应的 KV 值
			case <-rc.stopc:
				return false
			}

		case raftpb.EntryConfChange: // 将 EntryConfChange 类型的记录封装成 ConfChange
			var cc raftpb.ConfChange
			cc.Unmarshal(ents[i].Data)
			rc.confState = *rc.node.ApplyConfChange(cc) // 将 ConfChange 实例传入底层的 etcd-raft 组件，其中的处理过程在前面已经详细分析过了
			switch cc.Type { // 除了 etcd-raft 纽件中需要创建(或删除)对应的 Progress 实例 ，网络层也需要做出相应的调整，即添加(或删除)相应的 Peer 实例
			case raftpb.ConfChangeAddNode:
				if len(cc.Context) > 0 {
					rc.transport.AddPeer(types.ID(cc.NodeID), []string{string(cc.Context)})
				}
			case raftpb.ConfChangeRemoveNode:
				if cc.NodeID == uint64(rc.id) {
					log.Println("I've been removed from the cluster! Shutting down.")
					return false
				}
				rc.transport.RemovePeer(types.ID(cc.NodeID))
			}
		}

		// after commit, update appliedIndex // 处理完成之后，更新 raftNode 记录的已应用位置，该值在过滤已应用记录的 entriesToApply() 方法及后面即将介绍的 maybeTriggerSnapshot() 方法中都有使用
		rc.appliedIndex = ents[i].Index

		// special nil commit to signal replay has finished // 此次反用的是否为重放的 Entry 记录，如采是，且重放完成，则使用 cornrnitC 通道通知 kvstore
		if ents[i].Index == rc.lastIndex {
			select {
			case rc.commitC <- nil:
			case <-rc.stopc:
				return false
			}
		}
	}
	return true
}

func (rc *raftNode) loadSnapshot() *raftpb.Snapshot {
	snapshot, err := rc.snapshotter.Load()
	if err != nil && err != snap.ErrNoSnapshot {
		log.Fatalf("raftexample: error loading snapshot (%v)", err)
	}
	return snapshot
}

// openWAL returns a WAL ready for reading.
func (rc *raftNode) openWAL(snapshot *raftpb.Snapshot) *wal.WAL {
	if !wal.Exist(rc.waldir) { // 检测 WAL 日志目录是否存在，如果不存在进行创建
		if err := os.Mkdir(rc.waldir, 0750); err != nil {
			log.Fatalf("raftexample: cannot create dir for wal (%v)", err)
		}
		// 新建 WAL 实例，其中会创建相应目录和一个空的 WAL 日志文件
		w, err := wal.Create(zap.NewExample(), rc.waldir, nil)
		if err != nil {
			log.Fatalf("raftexample: create wal error (%v)", err)
		}
		w.Close()// 关闭 WAL, 其中包括各种关闭目录、文件和相关的 goroutine
	}

	walsnap := walpb.Snapshot{}
	if snapshot != nil {  // 创建 walsnap.Snapshot 实例并初始化其 Index 字段和 Term 字段
		walsnap.Index, walsnap.Term = snapshot.Metadata.Index, snapshot.Metadata.Term
	}
	log.Printf("loading WAL at term %d and index %d", walsnap.Term, walsnap.Index)
	w, err := wal.Open(zap.NewExample(), rc.waldir, walsnap)  // 创建WAL实例
	if err != nil {
		log.Fatalf("raftexample: error loading wal (%v)", err)
	}

	return w
}

// replayWAL replays WAL entries into the raft instance. 日志管理
func (rc *raftNode) replayWAL() *wal.WAL { // 重放节点 WAL 日志，以将重新初始化 raft 实例的内存状态
	log.Printf("replaying WAL of member %d", rc.id)
	snapshot := rc.loadSnapshot() // 1. 加载快照数据
	w := rc.openWAL(snapshot)   // 2. 借助快照数据（的相关属性）来打开 WAL 日志。
	_, st, ents, err := w.ReadAll() // 3. 从 WAL 日志中读取事务日志
	if err != nil {
		log.Fatalf("raftexample: failed to read WAL (%v)", err)
	}
	rc.raftStorage = raft.NewMemoryStorage()  // 4. 构建 raft 实例的内存状态结构
	if snapshot != nil {  // 5. 将快照数据直接加载应用到内存结构
		rc.raftStorage.ApplySnapshot(*snapshot)
	}
	rc.raftStorage.SetHardState(st)  // 将读取 WAL 日志之后得到的 HardState 加载到 MemoryStorage 中

	// append to storage so raft starts at the right place in log
	rc.raftStorage.Append(ents)  // 6. 将 WAL 记录的日志项更新到内存状态结构
	// send nil once lastIndex is published so client knows commit channel is current
	if len(ents) > 0 {  // 更新最后一条日志索引的记录，快照之后存在已经持久化的 Entry 记录，这些记录需要回放到上层应用的状态机中
		rc.lastIndex = ents[len(ents)-1].Index
	} else {
		rc.commitC <- nil // 快照之后不存在持久化的 Entry 记录，则向 commitC 中写入 nil，当 WAL 日志全部回放完成，也会向 commitC 写入 nil 作为信号
	}
	return w
}

func (rc *raftNode) writeError(err error) {
	rc.stopHTTP()
	close(rc.commitC)
	rc.errorC <- err
	close(rc.errorC)
	rc.node.Stop()
}

func (rc *raftNode) startRaft() {
	if !fileutil.Exist(rc.snapdir) {  // 若快照目录不存在，则创建
		if err := os.Mkdir(rc.snapdir, 0750); err != nil {
			log.Fatalf("raftexample: cannot create dir for snapshot (%v)", err)
		}
	}
	rc.snapshotter = snap.New(zap.NewExample(), rc.snapdir) // 创建 Snapshotter，并将该 Snapshotter 实例返回给上层模块
	rc.snapshotterReady <- rc.snapshotter
	// 2. 创建 WAL 实例，然后加载快照并回放 WAL 日志
	oldwal := wal.Exist(rc.waldir) // 判断是否已存在WAL日志（在节点当机重启时会执行）
	rc.wal = rc.replayWAL() // 重放WAL日志以应用到raft实例中
	// 3. 创建 raft.Config 实例
	rpeers := make([]raft.Peer, len(rc.peers))
	for i := range rpeers {  // 创建集群节点标识
		rpeers[i] = raft.Peer{ID: uint64(i + 1)}
	}
	c := &raft.Config{ // // 创建 raft.Config 实例，其中包含了启动 etcd-raft 模块的所有配置
		ID:                        uint64(rc.id),
		ElectionTick:              10, // 选举超时
		HeartbeatTick:             1, // 心跳超时
		Storage:                   rc.raftStorage, // 持久化存储。与 etcd-raft 模块中的 raftLog.storage 共享同一个 MemoryStorage 实例
		MaxSizePerMsg:             1024 * 1024, //每条消息的最大长度
		MaxInflightMsgs:           256, // 已发送但是未收到响应的消息上限个数
		MaxUncommittedEntriesSize: 1 << 30, // 最大未提交的条目
	}
	// 4. 初始化底层的 etcd-raft 模块，这里会根据 WAL 日志的回放情况判断当前节点是首次启动还是重新启动
	if oldwal { // 若已存在 WAL 日志，则重启节点（并非第一次启动）
		rc.node = raft.RestartNode(c)
	} else {
		startPeers := rpeers
		if rc.join { // 节点可以通过两种不同的方式来加入集群，应用以 join 字段来区分
			startPeers = nil
		}
		rc.node = raft.StartNode(c, startPeers) // 启动一个 Node
	}
	// 5. 创建 Transport 实例并启动，他负责 raft 节点之间的网络通信服务
	rc.transport = &rafthttp.Transport{
		Logger:      zap.NewExample(),
		ID:          types.ID(rc.id),
		ClusterID:   0x1000,
		Raft:        rc,
		ServerStats: stats.NewServerStats("", ""),
		LeaderStats: stats.NewLeaderStats(zap.NewExample(), strconv.Itoa(rc.id)),
		ErrorC:      make(chan error),
	}
	// 启动（初始化）transport 的相关内容
	rc.transport.Start()
	for i := range rc.peers {
		if i+1 != rc.id {  // 为每一个节点添加集群中其它的 peer，并且会启动数据传输通道
			rc.transport.AddPeer(types.ID(i+1), []string{rc.peers[i]})
		}
	}

	go rc.serveRaft()  // 本节点与其他节点相互通信的http服务监听
	go rc.serveChannels()  // 本节点与底层raft之间的通信模块
}

// stop closes http, closes all channels, and stops raft.
func (rc *raftNode) stop() {
	rc.stopHTTP()
	close(rc.commitC)
	close(rc.errorC)
	rc.node.Stop()
}

func (rc *raftNode) stopHTTP() {
	rc.transport.Stop()
	close(rc.httpstopc)
	<-rc.httpdonec
}
// 通知上层模块加载新生成的快照数据 ，并使用新快 照的元数据更新 raftNode 中的相应字段
func (rc *raftNode) publishSnapshot(snapshotToSave raftpb.Snapshot) {
	if raft.IsEmptySnap(snapshotToSave) {
		return
	} // 对快照数据进行一系列检测

	log.Printf("publishing snapshot at index %d", rc.snapshotIndex)
	defer log.Printf("finished publishing snapshot at index %d", rc.snapshotIndex)

	if snapshotToSave.Metadata.Index <= rc.appliedIndex {
		log.Fatalf("snapshot index [%d] should > progress.appliedIndex [%d]", snapshotToSave.Metadata.Index, rc.appliedIndex)
	}
	rc.commitC <- nil // trigger kvstore to load snapshot  // 使用 commitC 远远远知上层应用加载新 生成的快照数据

	rc.confState = snapshotToSave.Metadata.ConfState // 记录新快照的元数据
	rc.snapshotIndex = snapshotToSave.Metadata.Index
	rc.appliedIndex = snapshotToSave.Metadata.Index
}

var snapshotCatchUpEntriesN uint64 = 10000
// 快照管理
func (rc *raftNode) maybeTriggerSnapshot() {   // 1. 只有当前已经提交应用的日志的数据达到 rc.snapCount 才会触发快照操作
	if rc.appliedIndex-rc.snapshotIndex <= rc.snapCount {
		return
	}

	log.Printf("start snapshot [applied index: %d | last snapshot index: %d]", rc.appliedIndex, rc.snapshotIndex)
	data, err := rc.getSnapshot()  // 2.  获取快照数据，在 raftexample 示例中是获取 kvstore 中记录的全部键位对数据
	if err != nil {
		log.Panic(err)
	}
	snap, err := rc.raftStorage.CreateSnapshot(rc.appliedIndex, &rc.confState, data)   // 2.  创建 Snapshot 实例 同时也会将快照和元数据更新到 raftLog.MernoryStorage 中
	if err != nil {
		panic(err)
	}
	if err := rc.saveSnap(snap); err != nil {
		panic(err)
	}   // 4. 快照存盘。保存快照数据，raftNode.saveSnap() 方法在前面 已经介绍过了

	compactIndex := uint64(1)
	if rc.appliedIndex > snapshotCatchUpEntriesN { // 5. 判断是否达到阶段性整理内存日志的条件，若达到，则将内存中的数据进行阶段性整理标记。计算压缩的位置， 压缩之后，该位置之前的全部记录都会被抛弃
		compactIndex = rc.appliedIndex - snapshotCatchUpEntriesN
	}
	if err := rc.raftStorage.Compact(compactIndex); err != nil {
		panic(err)
	} // 压缩 raftLog 中保存的 Entry 记录

	log.Printf("compacted log at index %d", compactIndex)
	rc.snapshotIndex = rc.appliedIndex  // 6. 最后更新当前已快照的日志索引
}
// 单独启动一个后台 goroutine来负责上层模块 传递给 etcd-ra企 模块的数据， 主要 处理前面介绍的 proposeC、 confChangeC 两个通道 。
func (rc *raftNode) serveChannels() {
	snap, err := rc.raftStorage.Snapshot() // 获取快照数据和快照的元数据
	if err != nil {
		panic(err)
	}
	rc.confState = snap.Metadata.ConfState  // 利用 raft 实例的内存状态机初始化 snapshot 相关属性
	rc.snapshotIndex = snap.Metadata.Index
	rc.appliedIndex = snap.Metadata.Index

	defer rc.wal.Close()
	// 初始化一个定时器，每次触发 tick 都会调用底层 node.Tick()函数，以表示一次心跳事件，不同角色的事件处理函数不同。
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	// send proposals over raft。开启 go routine 以接收应用层(kvstore)的请求（包括正常的日志请求及集群配置变更请求）
	go func() {
		confChangeCount := uint64(0)
		// 循环监听来自 kvstore 的请求消息
		for rc.proposeC != nil && rc.confChangeC != nil {
			select {
			case prop, ok := <-rc.proposeC:// 1. 正常的日志请求 // 收到上层应用通过 proposeC put传递过来的数据
				if !ok {
					rc.proposeC = nil
				} else {
					// blocks until accepted by raft state machine// 通过 node.Propose()方法，将数据传入底层 etcd-raft 纽件进行处理
					rc.node.Propose(context.TODO(), []byte(prop))
				}
				// 收到上层应用通过 confChangeC POST 传递过来的数据
			case cc, ok := <-rc.confChangeC:// 2. 配置变更请求类似处理
				if !ok {
					rc.confChangeC = nil
				} else {
					confChangeCount++ // 统计集群变更请求的个数，并将其作为 ID
					cc.ID = confChangeCount
					rc.node.ProposeConfChange(context.TODO(), cc)  // 通过 node. ProposeConfChange() 方法，将数据传入反层 etcd-raft 纽件进行处理
				}
			}
		}
		// client closed channel; shutdown raft if not already //  关闭 stopc 通道，触发 rafeNode.stop() 方法的调用
		close(rc.stopc)
	}()
	// 开启 go routine 以循环处理底层 raft 核心库通过 Ready 通道发送给 raftNode 的指令
	// event loop on raft state machine updates // 该循环主要负责处理底层 etcd-raft 纽件返回的 Ready 数据
	for {
		select {// 触发定时器事件
		case <-ticker.C:
			rc.node.Tick()  // 上述 ticker 定时器触发一次，即会推进 etcd-raft 组件的逻辑时钟

		// store raft entries to wal, then publish over commit channel
		case rd := <-rc.node.Ready():  // 1.通过 Ready 获取 raft 核心库传递的指令
			rc.wal.Save(rd.HardState, rd.Entries)  // 2. 先写 WAL 日志
			if !raft.IsEmptySnap(rd.Snapshot) { // 检测到 etcd-raft 组件生成了新的快照数据
				rc.saveSnap(rd.Snapshot) // 将新的快照数据写入快照文件中
				rc.raftStorage.ApplySnapshot(rd.Snapshot)  // 将新快照持久化到 raftStorage, MemoryStorage 的实现在后面的章节详细介绍
				rc.publishSnapshot(rd.Snapshot)  // 通知上层应用加载新快照
			}
			rc.raftStorage.Append(rd.Entries) // 3. 更新 raft 实例的内存状态  // 将待持久化的 Entry 记录追加到 raftStorage 中完成持久化
			rc.transport.Send(rd.Messages) // 4. 将接收到消息传递通过 transport 组件传递给集群其它 peer, 发送心跳
			if ok := rc.publishEntries(rc.entriesToApply(rd.CommittedEntries)); !ok {  // 5. 将已提交、待应用的 Entry 记录应用到上层应用的状态机中，异常处理(略)
				rc.stop()
				return
			}
			rc.maybeTriggerSnapshot() // 6. 如果有必要，则会触发一次快照
			rc.node.Advance()  // 7. 通知底层 raft 核心库，当前的指令已经提交应用完成，这使得 raft 核心库可以发送下一个 Ready 指令了。

		case err := <-rc.transport.ErrorC:  // 处理网络异常
			rc.writeError(err) // 关闭与集群中其他节点的网络连接
			return

		case <-rc.stopc:
			rc.stop()  // 处理关闭命令
			return
		}
	}
}
//负责监听当前节点的地址，完成与其他节点的通信
func (rc *raftNode) serveRaft() {
	url, err := url.Parse(rc.peers[rc.id-1])  // 获取当前节点的 URL 地址
	if err != nil {
		log.Fatalf("raftexample: Failed parsing URL (%v)", err)
	}
	// 创建 stoppableListener 实例，stoppableListener 继承了 net.TCPListener 接口，它会与 http.Server 配合实现对当前节点的 URL 地址进行监听
	ln, err := newStoppableListener(url.Host, rc.httpstopc)
	if err != nil {
		log.Fatalf("raftexample: Failed to listen rafthttp (%v)", err)
	}
	// 创建 http.Server 实例，它会通过上面的 stoppableListener 实例监听当前的 URL 地址 stoppableListener.Accept() 方法监听到新的连接到来时，会创建对应的 net.Conn 实例，http.Server 会为每个连接创建单独的 goroutine 处理，每个请求都会由 http.Server.Handler 处理。
	err = (&http.Server{Handler: rc.transport.Handler()}).Serve(ln)
	select {
	case <-rc.httpstopc:
	default:
		log.Fatalf("raftexample: Failed to serve rafthttp (%v)", err)
	}
	close(rc.httpdonec)
}

func (rc *raftNode) Process(ctx context.Context, m raftpb.Message) error {
	return rc.node.Step(ctx, m)
}
func (rc *raftNode) IsIDRemoved(id uint64) bool                           { return false }
func (rc *raftNode) ReportUnreachable(id uint64)                          {}
func (rc *raftNode) ReportSnapshot(id uint64, status raft.SnapshotStatus) {}
