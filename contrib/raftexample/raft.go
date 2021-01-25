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
	proposeC    <-chan string            // proposed messages (k,v) 同kvStore.proposeC通道类似，事实上，kvStore会将用户的更新请求传递给raftNode以使得其最终能传递给底层的Raft协议库。
	confChangeC <-chan raftpb.ConfChange // proposed cluster config changes  Raft协议通过此channel来传递集群配置变更的请求给应用。
	commitC     chan<- *string           // entries committed to log (k,v) 底层Raft协议通过此channel可以向应用传递准备提交或应用的channel，最终kvStore会反复从此通道中读取可以提交的日志entry，然后正式应用到状态机。
	errorC      chan<- error             // errors from raft session

	id          int      // client ID for raft session
	peers       []string // raft peer URLs
	join        bool     // node is joining an existing cluster
	waldir      string   // path to WAL directory
	snapdir     string   // path to snapshot directory
	getSnapshot func() ([]byte, error)
	lastIndex   uint64 // index of log at start

	confState     raftpb.ConfState
	snapshotIndex uint64
	appliedIndex  uint64

	// raft backing for the commit/error channel
	node        raft.Node // 即底层 <b>Raft协议<b>组件，raftNode可以通过node提供的接口来与Raft组件进行交互。
	raftStorage *raft.MemoryStorage //  Raft协议的状态存储组件，应用在更新kvStore状态机时，也会更新此组件，并且通过raft.Config传给Raft协议。
	wal         *wal.WAL // 管理WAL日志，前文提过etcd将日志的相关逻辑交由应用来管理。

	snapshotter      *snap.Snapshotter // 管理 snapshot文件，快照文件也是由应用来管理。
	snapshotterReady chan *snap.Snapshotter // signals when snapshotter is ready

	snapCount uint64
	transport *rafthttp.Transport // 应用通过此接口与集群中其它的节点(peer)通信，比如传输日志同步消息、快照同步消息等。网络传输也是由应用来处理。
	stopc     chan struct{} // signals proposal channel closed
	httpstopc chan struct{} // signals http server to shutdown
	httpdonec chan struct{} // signals http server shutdown complete
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
		proposeC:    proposeC,
		confChangeC: confChangeC,
		commitC:     commitC,
		errorC:      errorC,
		id:          id,
		peers:       peers,
		join:        join,
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

func (rc *raftNode) saveSnap(snap raftpb.Snapshot) error {
	// must save the snapshot index to the WAL before saving the
	// snapshot to maintain the invariant that we only Open the
	// wal at previously-saved snapshot indexes.
	walSnap := walpb.Snapshot{
		Index: snap.Metadata.Index,
		Term:  snap.Metadata.Term,
	}
	if err := rc.wal.SaveSnapshot(walSnap); err != nil {
		return err
	}
	if err := rc.snapshotter.SaveSnap(snap); err != nil {
		return err
	}
	return rc.wal.ReleaseLockTo(snap.Metadata.Index)
}

func (rc *raftNode) entriesToApply(ents []raftpb.Entry) (nents []raftpb.Entry) {
	if len(ents) == 0 {
		return ents
	}
	firstIdx := ents[0].Index
	if firstIdx > rc.appliedIndex+1 {
		log.Fatalf("first index of committed entry[%d] should <= progress.appliedIndex[%d]+1", firstIdx, rc.appliedIndex)
	}
	if rc.appliedIndex-firstIdx+1 < uint64(len(ents)) {
		nents = ents[rc.appliedIndex-firstIdx+1:]
	}
	return nents
}

// publishEntries writes committed log entries to commit channel and returns
// whether all entries could be published.
func (rc *raftNode) publishEntries(ents []raftpb.Entry) bool {
	for i := range ents {
		switch ents[i].Type {
		case raftpb.EntryNormal:
			if len(ents[i].Data) == 0 {
				// ignore empty messages
				break
			}
			s := string(ents[i].Data)
			select {
			case rc.commitC <- &s:
			case <-rc.stopc:
				return false
			}

		case raftpb.EntryConfChange:
			var cc raftpb.ConfChange
			cc.Unmarshal(ents[i].Data)
			rc.confState = *rc.node.ApplyConfChange(cc)
			switch cc.Type {
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

		// after commit, update appliedIndex
		rc.appliedIndex = ents[i].Index

		// special nil commit to signal replay has finished
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
	if !wal.Exist(rc.waldir) {
		if err := os.Mkdir(rc.waldir, 0750); err != nil {
			log.Fatalf("raftexample: cannot create dir for wal (%v)", err)
		}

		w, err := wal.Create(zap.NewExample(), rc.waldir, nil)
		if err != nil {
			log.Fatalf("raftexample: create wal error (%v)", err)
		}
		w.Close()
	}

	walsnap := walpb.Snapshot{}
	if snapshot != nil {
		walsnap.Index, walsnap.Term = snapshot.Metadata.Index, snapshot.Metadata.Term
	}
	log.Printf("loading WAL at term %d and index %d", walsnap.Term, walsnap.Index)
	w, err := wal.Open(zap.NewExample(), rc.waldir, walsnap)
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
	rc.raftStorage.SetHardState(st)

	// append to storage so raft starts at the right place in log
	rc.raftStorage.Append(ents)  // 6. 将 WAL 记录的日志项更新到内存状态结构
	// send nil once lastIndex is published so client knows commit channel is current
	if len(ents) > 0 {  // 更新最后一条日志索引的记录
		rc.lastIndex = ents[len(ents)-1].Index
	} else {
		rc.commitC <- nil
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
	rc.snapshotter = snap.New(zap.NewExample(), rc.snapdir)
	rc.snapshotterReady <- rc.snapshotter

	oldwal := wal.Exist(rc.waldir) // 判断是否已存在WAL日志（在节点当机重启时会执行）
	rc.wal = rc.replayWAL() // 重放WAL日志以应用到raft实例中

	rpeers := make([]raft.Peer, len(rc.peers))
	for i := range rpeers {  // 创建集群节点标识
		rpeers[i] = raft.Peer{ID: uint64(i + 1)}
	}
	c := &raft.Config{ // // 初始化自身底层raft协议实例的配置结构
		ID:                        uint64(rc.id),
		ElectionTick:              10,
		HeartbeatTick:             1,
		Storage:                   rc.raftStorage,
		MaxSizePerMsg:             1024 * 1024,
		MaxInflightMsgs:           256,
		MaxUncommittedEntriesSize: 1 << 30,
	}
	// 启动底层raft
	if oldwal { // 若已存在 WAL 日志，则重启节点（并非第一次启动）
		rc.node = raft.RestartNode(c)
	} else {
		startPeers := rpeers
		if rc.join { // 节点可以通过两种不同的方式来加入集群，应用以 join 字段来区分
			startPeers = nil
		}
		rc.node = raft.StartNode(c, startPeers) // 启动一个 Node
	}
	// 初始化网络组件
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

func (rc *raftNode) publishSnapshot(snapshotToSave raftpb.Snapshot) {
	if raft.IsEmptySnap(snapshotToSave) {
		return
	}

	log.Printf("publishing snapshot at index %d", rc.snapshotIndex)
	defer log.Printf("finished publishing snapshot at index %d", rc.snapshotIndex)

	if snapshotToSave.Metadata.Index <= rc.appliedIndex {
		log.Fatalf("snapshot index [%d] should > progress.appliedIndex [%d]", snapshotToSave.Metadata.Index, rc.appliedIndex)
	}
	rc.commitC <- nil // trigger kvstore to load snapshot

	rc.confState = snapshotToSave.Metadata.ConfState
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
	data, err := rc.getSnapshot()  // 2. 生成此时应用的状态机的状态数据，此函数由应用提供，可以在 kvstore.go 找到它的定义
	if err != nil {
		log.Panic(err)
	}
	snap, err := rc.raftStorage.CreateSnapshot(rc.appliedIndex, &rc.confState, data)   // 2. 结合已经提交的日志以及配置状态数据正式生成快照
	if err != nil {
		panic(err)
	}
	if err := rc.saveSnap(snap); err != nil {
		panic(err)
	}   // 4. 快照存盘

	compactIndex := uint64(1)
	if rc.appliedIndex > snapshotCatchUpEntriesN { // 5. 判断是否达到阶段性整理内存日志的条件，若达到，则将内存中的数据进行阶段性整理标记
		compactIndex = rc.appliedIndex - snapshotCatchUpEntriesN
	}
	if err := rc.raftStorage.Compact(compactIndex); err != nil {
		panic(err)
	}

	log.Printf("compacted log at index %d", compactIndex)
	rc.snapshotIndex = rc.appliedIndex  // 6. 最后更新当前已快照的日志索引
}

func (rc *raftNode) serveChannels() {
	snap, err := rc.raftStorage.Snapshot()
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
			case prop, ok := <-rc.proposeC:// 1. 正常的日志请求
				if !ok {
					rc.proposeC = nil
				} else {
					// blocks until accepted by raft state machine// 调用底层的 raft 核心库的 node 的 Propose 接口来处理请求
					rc.node.Propose(context.TODO(), []byte(prop))
				}

			case cc, ok := <-rc.confChangeC:// 2. 配置变更请求类似处理
				if !ok {
					rc.confChangeC = nil
				} else {
					confChangeCount++
					cc.ID = confChangeCount
					rc.node.ProposeConfChange(context.TODO(), cc)
				}
			}
		}
		// client closed channel; shutdown raft if not already
		close(rc.stopc)
	}()
	// 开启 go routine 以循环处理底层 raft 核心库通过 Ready 通道发送给 raftNode 的指令
	// event loop on raft state machine updates
	for {
		select {// 触发定时器事件
		case <-ticker.C:
			rc.node.Tick()

		// store raft entries to wal, then publish over commit channel
		case rd := <-rc.node.Ready():  // 1.通过 Ready 获取 raft 核心库传递的指令
			rc.wal.Save(rd.HardState, rd.Entries)  // 2. 先写 WAL 日志
			if !raft.IsEmptySnap(rd.Snapshot) {
				rc.saveSnap(rd.Snapshot)
				rc.raftStorage.ApplySnapshot(rd.Snapshot)
				rc.publishSnapshot(rd.Snapshot)
			}
			rc.raftStorage.Append(rd.Entries) // 3. 更新 raft 实例的内存状态
			rc.transport.Send(rd.Messages) // 4. 将接收到消息传递通过 transport 组件传递给集群其它 peer
			if ok := rc.publishEntries(rc.entriesToApply(rd.CommittedEntries)); !ok {  // 5. 将已经提交的请求日志应用到状态机
				rc.stop()
				return
			}
			rc.maybeTriggerSnapshot() // 6. 如果有必要，则会触发一次快照
			rc.node.Advance()  // 7. 通知底层 raft 核心库，当前的指令已经提交应用完成，这使得 raft 核心库可以发送下一个 Ready 指令了。

		case err := <-rc.transport.ErrorC:
			rc.writeError(err)
			return

		case <-rc.stopc:
			rc.stop()
			return
		}
	}
}

func (rc *raftNode) serveRaft() {
	url, err := url.Parse(rc.peers[rc.id-1])
	if err != nil {
		log.Fatalf("raftexample: Failed parsing URL (%v)", err)
	}

	ln, err := newStoppableListener(url.Host, rc.httpstopc)
	if err != nil {
		log.Fatalf("raftexample: Failed to listen rafthttp (%v)", err)
	}

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
